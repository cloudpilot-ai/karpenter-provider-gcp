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

// Package sandbox resolves public artifacts. It never executes downloaded code.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	InstallerRepository = "gke.gcr.io/gke-gvisor-installer"
	artifactTTL         = 5 * time.Minute
	maxCacheEntries     = 256
)

type cachedArtifact struct {
	value   any
	expires time.Time
}

// Resolver uses fixed public origins, bounded reads and positive-only caching.
// A resolver should be shared across image and bootstrap reconciliation.
type Resolver struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cachedArtifact
}

func NewResolver() *Resolver {
	return &Resolver{client: &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host || req.URL.User != nil {
				return fmt.Errorf("public artifact redirect must remain on the original HTTPS origin")
			}
			// A shared storage origin also hosts unrelated buckets. Bootstrap
			// redirects must not escape the validated versioned GKE artifact.
			if via[0].URL.Host == "storage.googleapis.com" && req.URL.String() != via[0].URL.String() {
				return fmt.Errorf("bootstrap redirect must retain the versioned public GKE artifact path")
			}
			return nil
		},
	}, cache: map[string]cachedArtifact{}}
}

func (r *Resolver) get(key string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.cache[key]
	if ok && time.Now().Before(v.expires) {
		return v.value, true
	}
	delete(r.cache, key)
	return nil, false
}
func (r *Resolver) put(key string, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) >= maxCacheEntries {
		var oldest string
		var expiry time.Time
		for k, v := range r.cache {
			if oldest == "" || v.expires.Before(expiry) {
				oldest, expiry = k, v.expires
			}
		}
		delete(r.cache, oldest)
	}
	r.cache[key] = cachedArtifact{value: value, expires: time.Now().Add(artifactTTL)}
}
func (r *Resolver) fetch(ctx context.Context, url, accept string, limit int64) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", accept)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("public artifact returned HTTP %d; verify the selected release/image has a published artifact and outbound access is permitted", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(b)) > limit {
		return nil, nil, fmt.Errorf("public artifact exceeds %d byte limit", limit)
	}
	return b, resp.Header, nil
}
func digestBytes(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

// SharedResolver reuses artifact/profile validation across image readiness and
// launches. It retains only bounded public artifact identities, never kube-env.
var sharedResolver = NewResolver()

func SharedResolver() *Resolver { return sharedResolver }

type BootstrapResolver interface {
	ResolveBootstrap(context.Context, string, string) (BootstrapProfile, error)
}
