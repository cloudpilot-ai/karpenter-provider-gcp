/*
Copyright 2025 The CloudPilot AI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metadata

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
)

var (
	// serverBinaryArchRegex matches the architecture portion of a GKE binary URL.
	serverBinaryArchRegex = regexp.MustCompile(`kubernetes-server-linux-(amd64|arm64)\.tar\.gz`)
	// gkeReleaseVersionRegex extracts the GKE release version from a binary URL.
	gkeReleaseVersionRegex = regexp.MustCompile(`/release/(v[^/]+)/`)
)

// archHashCache caches GCS-fetched SHA-512 hashes keyed by "arch:version".
var archHashCache sync.Map

// archHashHTTPTimeout is the per-request deadline for fetching a GKE binary hash from GCS.
const archHashHTTPTimeout = 30 * time.Second

// PatchKubeEnvForArch patches SERVER_BINARY_TAR_URL and SERVER_BINARY_TAR_HASH in the
// kube-env metadata item so that the target node bootstraps with the correct binary for
// its architecture, regardless of the source pool's architecture.
//
// It is a no-op when the source URL already carries the target architecture.
// The SHA-512 hash for the target binary is fetched from the public GCS sidecar file
// and cached by "arch:version" to avoid repeated network calls.
//
// gkeVersion is used only as a fallback when the version cannot be parsed from the URL.
// Pass an empty string to rely solely on the URL-parsing behavior (useful for tests).
func PatchKubeEnvForArch(ctx context.Context, metaData *compute.Metadata, targetArch, gkeVersion string, httpClient *http.Client) error {
	for _, item := range metaData.Items {
		if item.Key != "kube-env" {
			continue
		}
		kubeEnv := lo.FromPtr(item.Value)
		if kubeEnv == "" {
			return fmt.Errorf("kube-env metadata is empty")
		}
		updated, err := patchServerBinaryForArch(ctx, kubeEnv, targetArch, gkeVersion, httpClient)
		if err != nil {
			return err
		}
		item.Value = lo.ToPtr(updated)
	}
	return nil
}

// patchServerBinaryForArch performs the actual string replacement on a kube-env string.
func patchServerBinaryForArch(ctx context.Context, kubeEnv, targetArch, gkeVersion string, httpClient *http.Client) (string, error) {
	urlMatch := serverBinaryArchRegex.FindStringSubmatch(kubeEnv)
	if urlMatch == nil {
		return kubeEnv, nil
	}
	sourceArch := urlMatch[1]
	if sourceArch == targetArch {
		return kubeEnv, nil
	}

	// Always prefer the version embedded in the source URL so the fetched hash matches
	// the binary the URL points to. Fall back to caller-supplied gkeVersion only when
	// the URL cannot be parsed (e.g. during a control-plane upgrade).
	version := gkeVersion
	if m := gkeReleaseVersionRegex.FindStringSubmatch(kubeEnv); m != nil {
		version = m[1]
	} else if version == "" {
		return "", fmt.Errorf("could not extract GKE release version from SERVER_BINARY_TAR_URL")
	}

	hash, err := getArchHash(ctx, version, targetArch, httpClient)
	if err != nil {
		return "", fmt.Errorf("fetching %s binary hash for version %s: %w", targetArch, version, err)
	}

	updated := strings.ReplaceAll(kubeEnv,
		"linux-"+sourceArch+".tar.gz",
		"linux-"+targetArch+".tar.gz")

	lines := strings.Split(updated, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "SERVER_BINARY_TAR_HASH:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + "SERVER_BINARY_TAR_HASH: " + hash
		}
	}
	return strings.Join(lines, "\n"), nil
}

// getArchHash returns the SHA-512 hash for the given GKE binary tarball, fetching
// it from the public GCS sidecar file if not already cached.
func getArchHash(ctx context.Context, version, arch string, httpClient *http.Client) (string, error) {
	cacheKey := arch + ":" + version
	if cached, ok := archHashCache.Load(cacheKey); ok {
		return cached.(string), nil
	}

	url := fmt.Sprintf(
		"https://storage.googleapis.com/gke-release/kubernetes/release/%s/kubernetes-server-linux-%s.tar.gz.sha512",
		version, arch,
	)
	reqCtx, cancel := context.WithTimeout(ctx, archHashHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s returned %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading hash response: %w", err)
	}
	hash := strings.TrimSpace(string(body))
	if len(hash) != 128 {
		return "", fmt.Errorf("unexpected hash length %d (want 128) from %s", len(hash), url)
	}

	archHashCache.Store(cacheKey, hash)
	return hash, nil
}
