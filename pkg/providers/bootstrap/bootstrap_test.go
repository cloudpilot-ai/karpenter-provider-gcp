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

package bootstrap

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func fixtureObjects() map[string]string {
	objects := map[string]string{
		"kubernetes-server-linux-amd64.tar.gz.sha512": strings.Repeat("a", 128),
		"kubernetes-server-linux-arm64.tar.gz.sha512": strings.Repeat("b", 128),
		"kubernetes-manifests.tar.gz.sha512":          strings.Repeat("c", 128),
		"extra/gce/configure.sh":                      "#!/bin/bash\necho fixture\n",
		"extra/gce/node.yaml":                         "#cloud-config\nwrite_files: []\n",
	}
	for _, name := range []string{"extra/gce/configure.sh", "extra/gce/node.yaml"} {
		hash := sha512.Sum512([]byte(objects[name]))
		objects[name+".sha512"] = hex.EncodeToString(hash[:])
	}
	return objects
}

func fixtureClient(t *testing.T, objects map[string]string, visit func(string)) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		const prefix = "/gke-release/kubernetes/release/v1.36.4-gke.2046003/"
		if request.Method != "GET" || request.URL.Scheme != "https" || request.URL.Host != "storage.googleapis.com" || !strings.HasPrefix(request.URL.Path, prefix) {
			return nil, fmt.Errorf("unexpected bootstrap request %s %s", request.Method, request.URL)
		}
		name := strings.TrimPrefix(request.URL.Path, prefix)
		if visit != nil {
			visit(name)
		}
		body, ok := objects[name]
		status := http.StatusOK
		if !ok {
			status = http.StatusNotFound
			body = "fixture object not found"
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
}

func TestGetResolvesCoherentArchitectureSpecificBundleAndCachesNormalizedRelease(t *testing.T) {
	objects := fixtureObjects()
	requests := 0
	provider := NewDefaultProvider(fixtureClient(t, objects, func(string) { requests++ }))
	for _, test := range []struct{ arch, hash string }{{"amd64", strings.Repeat("a", 128)}, {"arm64", strings.Repeat("b", 128)}} {
		bundle, err := provider.Get(context.Background(), "v1.36.4-gke.2046003", test.arch)
		require.NoError(t, err)
		require.Equal(t, "1.36.4-gke.2046003", bundle.Version)
		require.Equal(t, "https://storage.googleapis.com/gke-release/kubernetes/release/v1.36.4-gke.2046003/kubernetes-server-linux-"+test.arch+".tar.gz", bundle.ServerBinaryURL)
		require.Equal(t, test.hash, bundle.ServerBinaryHash)
		require.Equal(t, "https://storage.googleapis.com/gke-release/kubernetes/release/v1.36.4-gke.2046003/kubernetes-manifests.tar.gz", bundle.ManifestsURL)
		require.Equal(t, strings.Repeat("c", 128), bundle.ManifestsHash)
		require.Equal(t, objects["extra/gce/configure.sh"], bundle.ConfigureScript)
		require.Equal(t, objects["extra/gce/node.yaml"], bundle.UserData)
		cached, err := provider.Get(context.Background(), "1.36.4-gke.2046003", test.arch)
		require.NoError(t, err)
		require.Equal(t, bundle, cached)
	}
	require.Equal(t, 12, requests, "only six small objects per architecture; no archive downloads or cached lookups")
}

func TestGetRejectsIncompleteOrCorruptBundleAndRetries(t *testing.T) {
	for _, test := range []struct {
		name         string
		breakObjects func(map[string]string)
		want         string
	}{
		{"missing manifests hash", func(objects map[string]string) { delete(objects, "kubernetes-manifests.tar.gz.sha512") }, "HTTP 404"},
		{"invalid server hash", func(objects map[string]string) { objects["kubernetes-server-linux-amd64.tar.gz.sha512"] = "not-sha512" }, "invalid SHA-512"},
		{"oversize hash response", func(objects map[string]string) {
			objects["kubernetes-server-linux-amd64.tar.gz.sha512"] = strings.Repeat("a", maxHashBytes+1)
		}, "exceeds"},
		{"script mismatch", func(objects map[string]string) { objects["extra/gce/configure.sh"] += "corruption" }, "checksum mismatch"},
		{"script response limit", func(objects map[string]string) {
			objects["extra/gce/node.yaml"] = strings.Repeat("x", maxScriptBytes+1)
		}, "exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := fixtureObjects()
			test.breakObjects(objects)
			provider := NewDefaultProvider(fixtureClient(t, objects, nil))
			_, err := provider.Get(context.Background(), "1.36.4-gke.2046003", "amd64")
			require.ErrorContains(t, err, test.want)
			// Fix the same objects. The failed tuple must not poison the cache.
			clear(objects)
			for key, value := range fixtureObjects() {
				objects[key] = value
			}
			bundle, err := provider.Get(context.Background(), "1.36.4-gke.2046003", "amd64")
			require.NoError(t, err)
			require.Equal(t, "1.36.4-gke.2046003", bundle.Version)
		})
	}
}

// Signals that the waiter has reached its cancellation select after joining the lookup.
type waitingContext struct {
	context.Context
	waiting func()
}

func (ctx waitingContext) Done() <-chan struct{} {
	ctx.waiting()
	return ctx.Context.Done()
}

func TestCanceledCallerDoesNotCancelSharedLookup(t *testing.T) {
	client := fixtureClient(t, fixtureObjects(), nil)
	transport := client.Transport
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var requests atomic.Int32
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			close(started)
			<-release
		}
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		return transport.RoundTrip(request)
	})
	provider := NewDefaultProvider(client)
	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstResult := make(chan error, 1)
	go func() {
		_, err := provider.Get(first, "1.36.4-gke.2046003", "amd64")
		firstResult <- err
	}()
	<-started

	second, cancelSecond := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSecond()
	joined := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		_, err := provider.Get(waitingContext{second, sync.OnceFunc(func() { close(joined) })}, "1.36.4-gke.2046003", "amd64")
		secondResult <- err
	}()
	<-joined
	cancelFirst()
	require.ErrorIs(t, <-firstResult, context.Canceled)
	unblock()
	require.NoError(t, <-secondResult)
	require.Equal(t, int32(6), requests.Load(), "the remaining caller completes the same shared bundle")
}

func TestGetRejectsUntrustedReleaseOrUnsupportedArchitectureBeforeHTTP(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid inputs must not reach HTTP")
		return nil, nil
	})}
	provider := NewDefaultProvider(client)
	for _, test := range []struct{ version, arch string }{
		{"", "amd64"}, {"1.36.4-gke.2046003/../../other", "amd64"},
		{"https://untrusted.example/release", "amd64"}, {"1.36.4-gke.2046003", "riscv64"},
	} {
		_, err := provider.Get(context.Background(), test.version, test.arch)
		require.Error(t, err)
	}
}
