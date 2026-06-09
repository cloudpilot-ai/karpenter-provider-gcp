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

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

var (
	osDistributionCOSRegex    = regexp.MustCompile(`\bgke-os-distribution=cos\b`)
	osDistributionUbuntuRegex = regexp.MustCompile(`\bgke-os-distribution=ubuntu\b`)
)

func replaceKubeEnvLabelAssignments(kubeEnv string, labelValues map[string]string) string {
	updated := kubeEnv
	for key, value := range labelValues {
		updated = replaceDelimitedAssignment(updated, key, value)
	}
	return updated
}

func replaceDelimitedAssignment(raw, key, value string) string {
	if key == "" {
		return raw
	}
	needle := key + "="
	var out strings.Builder
	for i := 0; i < len(raw); {
		idx := strings.Index(raw[i:], needle)
		if idx == -1 {
			out.WriteString(raw[i:])
			break
		}
		idx += i
		if idx > 0 && !isAssignmentDelimiter(raw[idx-1]) {
			out.WriteString(raw[i : idx+len(needle)])
			i = idx + len(needle)
			continue
		}
		end := idx + len(needle)
		for end < len(raw) && !isAssignmentDelimiter(raw[end]) {
			end++
		}
		out.WriteString(raw[i:idx])
		out.WriteString(needle)
		out.WriteString(value)
		i = end
	}
	return out.String()
}

func isAssignmentDelimiter(b byte) bool {
	return b == ',' || b == ';' || b == ' ' || b == '\t' || b == '\n'
}

// PatchKubeEnvForOSType patches the kube-env metadata item so a single bootstrap source pool can serve nodes of either OS family.
// Patching is bidirectional: Ubuntu→COS and COS→Ubuntu.
//
// Fields modified for Ubuntu target (source is COS):
//   - gke-os-distribution=cos → gke-os-distribution=ubuntu
//   - ENABLE_NODE_BFQ_IO_SCHEDULER line removed
//   - NODE_BFQ_IO_SCHEDULER_IO_WEIGHT line removed
//
// Fields modified for ContainerOptimizedOS target (source is Ubuntu):
//   - gke-os-distribution=ubuntu → gke-os-distribution=cos
//   - ENABLE_NODE_BFQ_IO_SCHEDULER: "true" added (if absent)
//   - NODE_BFQ_IO_SCHEDULER_IO_WEIGHT: "1200" added (if absent)
func PatchKubeEnvForOSType(values InstanceMetadata, imageFamily string) error {
	kubeEnv, ok := values[KubeEnvKey]
	if !ok {
		return nil
	}
	if kubeEnv == "" {
		return fmt.Errorf("kube-env metadata is empty")
	}

	var updated string
	switch imageFamily {
	case v1alpha1.ImageFamilyUbuntu:
		updated = osDistributionCOSRegex.ReplaceAllString(kubeEnv, "gke-os-distribution=ubuntu")
		updated = removeKubeEnvLine(updated, "ENABLE_NODE_BFQ_IO_SCHEDULER")
		updated = removeKubeEnvLine(updated, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT")
	case v1alpha1.ImageFamilyContainerOptimizedOS:
		updated = osDistributionUbuntuRegex.ReplaceAllString(kubeEnv, "gke-os-distribution=cos")
		updated = ensureKubeEnvLine(updated, "ENABLE_NODE_BFQ_IO_SCHEDULER", `"true"`)
		updated = ensureKubeEnvLine(updated, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT", `"1200"`)
	default:
		return nil
	}
	values[KubeEnvKey] = updated
	return nil
}

// ensureKubeEnvLine adds "key: value" to the kube-env string if no line with that key
// already exists. This is used when patching from an Ubuntu source template to a COS node.
func ensureKubeEnvLine(kubeEnv, key, value string) string {
	for _, line := range strings.Split(kubeEnv, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+":") {
			return kubeEnv
		}
	}
	return kubeEnv + "\n" + key + ": " + value
}

// removeKubeEnvLine removes any line whose key (text before the first colon) matches
// the given key. This handles the kube-env YAML-style "KEY: value" line format.
func removeKubeEnvLine(kubeEnv, key string) string {
	lines := strings.Split(kubeEnv, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+":") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

// SetKubeEnvZone patches or appends the ZONE field in kube-env metadata.
func SetKubeEnvZone(values InstanceMetadata, zone string) error {
	if values == nil || zone == "" {
		return fmt.Errorf("metadata and zone must be non-empty")
	}
	kubeEnv, ok := values[KubeEnvKey]
	if !ok {
		return fmt.Errorf("kube-env metadata key not found in instance template")
	}
	if kubeEnv == "" {
		return fmt.Errorf("kube-env metadata is empty")
	}
	zoneLine := "ZONE: " + zone
	zoneRegex := regexp.MustCompile(`(?m)^ZONE: [^\n]+`)
	if zoneRegex.MatchString(kubeEnv) {
		values[KubeEnvKey] = zoneRegex.ReplaceAllString(kubeEnv, zoneLine)
		return nil
	}
	values[KubeEnvKey] = strings.TrimRight(kubeEnv, "\n") + "\n" + zoneLine + "\n"
	return nil
}

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
func PatchKubeEnvForArch(ctx context.Context, values InstanceMetadata, targetArch, gkeVersion string, httpClient *http.Client) error {
	kubeEnv, ok := values[KubeEnvKey]
	if !ok {
		return nil
	}
	if kubeEnv == "" {
		return fmt.Errorf("kube-env metadata is empty")
	}
	updated, err := patchServerBinaryForArch(ctx, kubeEnv, targetArch, gkeVersion, httpClient)
	if err != nil {
		return err
	}
	values[KubeEnvKey] = updated
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
	hashPatched := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "SERVER_BINARY_TAR_HASH:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + "SERVER_BINARY_TAR_HASH: " + hash
			hashPatched = true
		}
	}
	if !hashPatched {
		return "", fmt.Errorf("SERVER_BINARY_TAR_HASH not found in kube-env after URL patch")
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
