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

// Package bootstrap resolves the core bootstrap artifacts of an already selected
// GKE release. It does not choose images or versions, read cluster credentials,
// discover source templates, or manage node pools and instances.
package bootstrap

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
	"k8s.io/utils/lru"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/metadata"
)

const (
	releaseRoot    = "https://storage.googleapis.com/gke-release/kubernetes/release/"
	lookupTimeout  = 45 * time.Second
	maxHashBytes   = 1024
	maxScriptBytes = 256 * 1024
)

var releasePattern = regexp.MustCompile(`^v?[1-9][0-9]*\.[0-9]+\.[0-9]+-gke\.[0-9]+$`)

type Provider interface {
	Get(context.Context, string, string) (metadata.BootstrapRelease, error)
}

type DefaultProvider struct {
	client   *http.Client
	cache    *lru.Cache
	requests singleflight.Group
}

func NewDefaultProvider(client *http.Client) *DefaultProvider {
	if client == nil {
		client = http.DefaultClient
	}
	return &DefaultProvider{client: client, cache: lru.New(64)}
}

// Get caches only complete, verified bundles. A release's URLs and scripts are
// kept together; failed lookups never fall back to source-template values.
func (p *DefaultProvider) Get(ctx context.Context, version, arch string) (metadata.BootstrapRelease, error) {
	if !releasePattern.MatchString(version) {
		return metadata.BootstrapRelease{}, fmt.Errorf("invalid GKE bootstrap release %q", version)
	}
	if arch != "amd64" && arch != "arm64" {
		return metadata.BootstrapRelease{}, fmt.Errorf("unsupported bootstrap architecture %q", arch)
	}
	version = strings.TrimPrefix(version, "v")
	key := version + "/" + arch
	if cached, ok := p.cache.Get(key); ok {
		return cached.(metadata.BootstrapRelease), nil
	}
	result := p.requests.DoChan(key, func() (any, error) {
		if cached, ok := p.cache.Get(key); ok {
			return cached.(metadata.BootstrapRelease), nil
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lookupTimeout)
		defer cancel()
		bundle, err := p.resolve(ctx, version, arch)
		if err != nil {
			return nil, err
		}
		p.cache.Add(key, bundle)
		return bundle, nil
	})
	select {
	case <-ctx.Done():
		return metadata.BootstrapRelease{}, ctx.Err()
	case response := <-result:
		if response.Err != nil {
			return metadata.BootstrapRelease{}, response.Err
		}
		return response.Val.(metadata.BootstrapRelease), nil
	}
}

func (p *DefaultProvider) resolve(ctx context.Context, version, arch string) (metadata.BootstrapRelease, error) {
	base := releaseRoot + "v" + version + "/"
	bundle := metadata.BootstrapRelease{
		Version:         version,
		ServerBinaryURL: base + "kubernetes-server-linux-" + arch + ".tar.gz",
		ManifestsURL:    base + "kubernetes-manifests.tar.gz",
	}
	var err error
	if bundle.ServerBinaryHash, err = p.hash(ctx, bundle.ServerBinaryURL); err != nil {
		return metadata.BootstrapRelease{}, err
	}
	if bundle.ManifestsHash, err = p.hash(ctx, bundle.ManifestsURL); err != nil {
		return metadata.BootstrapRelease{}, err
	}
	if bundle.ConfigureScript, err = p.script(ctx, base+"extra/gce/configure.sh"); err != nil {
		return metadata.BootstrapRelease{}, err
	}
	if bundle.UserData, err = p.script(ctx, base+"extra/gce/node.yaml"); err != nil {
		return metadata.BootstrapRelease{}, err
	}
	return bundle, nil
}

func (p *DefaultProvider) hash(ctx context.Context, url string) (string, error) {
	body, err := p.read(ctx, url+".sha512", maxHashBytes)
	if err != nil {
		return "", err
	}
	hash := strings.TrimSpace(string(body))
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha512.Size {
		return "", fmt.Errorf("invalid SHA-512 sidecar for %s", url)
	}
	return strings.ToLower(hash), nil
}

func (p *DefaultProvider) script(ctx context.Context, url string) (string, error) {
	hash, err := p.hash(ctx, url)
	if err != nil {
		return "", err
	}
	body, err := p.read(ctx, url, maxScriptBytes)
	if err != nil {
		return "", err
	}
	actual := sha512.Sum512(body)
	if len(body) == 0 || hex.EncodeToString(actual[:]) != hash {
		return "", fmt.Errorf("bootstrap script checksum mismatch for %s", url)
	}
	return string(body), nil
}

func (p *DefaultProvider) read(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting bootstrap artifact %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getting bootstrap artifact %s: HTTP %d", url, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading bootstrap artifact %s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("bootstrap artifact %s exceeds %d bytes", url, limit)
	}
	return body, nil
}
