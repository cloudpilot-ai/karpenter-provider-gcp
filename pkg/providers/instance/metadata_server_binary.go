/*
Copyright 2026 The CloudPilot AI Authors.

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

package instance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

var (
	serverBinaryArchRegex  = regexp.MustCompile(`kubernetes-server-linux-(amd64|arm64)\.tar\.gz`)
	gkeReleaseVersionRegex = regexp.MustCompile(`/release/(v[^/]+)/`)
	serverBinaryHashCache  sync.Map
)

const archHashHTTPTimeout = 30 * time.Second

func (p *DefaultProvider) patchKubeEnvServerBinaryForArch(ctx context.Context, target *metadata.InstanceMetadata, instanceType *cloudprovider.InstanceType) error {
	url, ok := target.GetKubeEnvEntry("SERVER_BINARY_TAR_URL")
	if !ok || url == "" {
		return nil
	}
	arch := instanceType.Requirements.Get("kubernetes.io/arch").Any()
	if arch == "" {
		arch = "amd64"
	}
	match := serverBinaryArchRegex.FindStringSubmatch(url)
	if match == nil {
		return nil
	}
	sourceArch := match[1]
	if sourceArch == arch {
		return nil
	}
	version := p.resolveGKEVersion(ctx)
	if m := gkeReleaseVersionRegex.FindStringSubmatch(url); m != nil {
		version = m[1]
	} else if version == "" {
		return fmt.Errorf("could not extract GKE release version from SERVER_BINARY_TAR_URL")
	}
	hash, err := getServerBinaryHash(ctx, version, arch, http.DefaultClient)
	if err != nil {
		return fmt.Errorf("fetching %s binary hash for version %s: %w", arch, version, err)
	}
	target.SetKubeEnvEntry("SERVER_BINARY_TAR_URL", strings.ReplaceAll(url, "linux-"+sourceArch+".tar.gz", "linux-"+arch+".tar.gz"))
	target.SetKubeEnvEntry("SERVER_BINARY_TAR_HASH", hash)
	return nil
}

func getServerBinaryHash(ctx context.Context, version, arch string, httpClient *http.Client) (string, error) {
	cacheKey := arch + ":" + version
	if cached, ok := serverBinaryHashCache.Load(cacheKey); ok {
		return cached.(string), nil
	}
	url := fmt.Sprintf("https://storage.googleapis.com/gke-release/kubernetes/release/%s/kubernetes-server-linux-%s.tar.gz.sha512", version, arch)
	client := httpClient
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, archHashHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty hash response")
	}
	hash := fields[0]
	serverBinaryHashCache.Store(cacheKey, hash)
	return hash, nil
}
