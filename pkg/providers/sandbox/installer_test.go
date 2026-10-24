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

package sandbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type artifactTransport func(*http.Request) (*http.Response, error)

func (f artifactTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func artifactResponse(body string, status int, mediaType string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {mediaType}, "Docker-Content-Digest": {fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))}}, Body: io.NopCloser(strings.NewReader(body))}
}
func publicIndex(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/installer-index.json")
	require.NoError(t, err)
	return string(b)
}

func TestInstallerVersion(t *testing.T) {
	version, err := InstallerVersion("20260323-0_rc03")
	require.NoError(t, err)
	require.Equal(t, "20260323.0_RC03", version)
	for _, label := range []string{"", "latest", "20260323.0_RC03", "20260230-0_rc03", "20260323-0_rc03/other", "20260323-0_rc3"} {
		_, err := InstallerVersion(label)
		require.Error(t, err, label)
	}
}

func TestResolveInstallerPublicIdentityAndRefresh(t *testing.T) {
	r := NewResolver()
	requests := 0
	r.client.Transport = artifactTransport(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "https://gke.gcr.io/v2/gke-gvisor-installer/manifests/20260323.0_RC03", req.URL.String())
		require.Contains(t, req.Header.Get("Accept"), ociIndex)
		deadline, ok := req.Context().Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
		requests++
		return artifactResponse(publicIndex(t), http.StatusOK, dockerIndex), nil
	})
	for _, arch := range []string{"amd64", "arm64"} {
		got, err := r.ResolveInstaller(context.Background(), "20260323-0_rc03", arch)
		require.NoError(t, err)
		require.Equal(t, "20260323.0_RC03", got.Version)
		require.Equal(t, "sha256:b70f646b07cdf451944c3120f63645f0b89baa9e2eb50bf784d7b2b8d6b47385", got.Digest)
		require.Equal(t, "gke.gcr.io/gke-gvisor-installer@"+got.Digest, InstallerReference(*got))
	}
	_, err := r.ResolveInstaller(context.Background(), "20260323-0_rc03", "amd64")
	require.NoError(t, err)
	require.Equal(t, 2, requests)
	// Expire the cache without sleeping; a renewed registry read is observable.
	for k, v := range r.cache {
		v.expires = time.Now().Add(-time.Minute)
		r.cache[k] = v
	}
	_, err = r.ResolveInstaller(context.Background(), "20260323-0_rc03", "amd64")
	require.NoError(t, err)
	require.Equal(t, 3, requests)
}

func TestResolveInstallerRejectsInvalidArtifactsAndDoesNotCacheFailure(t *testing.T) {
	index := publicIndex(t)
	cases := []struct {
		name, body, contentType, header string
		status                          int
	}{
		{name: "missing", body: "not found", status: 404, contentType: dockerIndex},
		{name: "digest mismatch", body: index, status: 200, contentType: dockerIndex, header: "sha256:" + strings.Repeat("0", 64)},
		{name: "missing digest", body: index, status: 200, contentType: dockerIndex, header: "absent"},
		{name: "media type", body: index, status: 200, contentType: "application/json"},
		{name: "schema", body: strings.Replace(index, `"schemaVersion": 2`, `"schemaVersion": 1`, 1), status: 200, contentType: dockerIndex},
		{name: "platform", body: strings.ReplaceAll(index, `"linux"`, `"windows"`), status: 200, contentType: dockerIndex},
		{name: "descriptor", body: strings.Replace(index, "sha256:554bc", "sha512:554bc", 1), status: 200, contentType: dockerIndex},
		{name: "body bound", body: strings.Repeat(" ", maxManifestBytes+1), status: 200, contentType: dockerIndex},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver()
			valid := false
			r.client.Transport = artifactTransport(func(_ *http.Request) (*http.Response, error) {
				if valid {
					return artifactResponse(index, 200, dockerIndex), nil
				}
				resp := artifactResponse(tt.body, tt.status, tt.contentType)
				if tt.header == "absent" {
					resp.Header.Del("Docker-Content-Digest")
				} else if tt.header != "" {
					resp.Header.Set("Docker-Content-Digest", tt.header)
				}
				return resp, nil
			})
			_, err := r.ResolveInstaller(context.Background(), "20260323-0_rc03", "amd64")
			require.Error(t, err)
			valid = true
			_, err = r.ResolveInstaller(context.Background(), "20260323-0_rc03", "amd64")
			require.NoError(t, err)
		})
	}
}

func TestPublicArtifactRedirectsStayOnOrigin(t *testing.T) {
	for _, destination := range []string{"https://untrusted.example/artifact", "http://gke.gcr.io/artifact", "https://gke.gcr.io:444/artifact"} {
		r := NewResolver()
		requests := 0
		r.client.Transport = artifactTransport(func(req *http.Request) (*http.Response, error) {
			requests++
			resp := artifactResponse("", 302, "")
			resp.Header.Set("Location", destination)
			return resp, nil
		})
		_, err := r.ResolveInstaller(context.Background(), "20260323-0_rc03", "amd64")
		require.Error(t, err)
		require.Equal(t, 1, requests)
	}
}

func TestResolveSingleManifestValidatesConfigPlatform(t *testing.T) {
	config := `{"os":"linux","architecture":"arm64"}`
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":%d,"digest":"sha256:%x"},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","size":123,"digest":"sha256:%s"}]}`, ociManifest, len(config), sha256.Sum256([]byte(config)), strings.Repeat("a", 64))
	r := NewResolver()
	r.client.Transport = artifactTransport(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/blobs/") {
			return artifactResponse(config, 200, "application/json"), nil
		}
		return artifactResponse(manifest, 200, ociManifest), nil
	})
	_, err := r.ResolveInstaller(context.Background(), "20260323-0_rc03", "amd64")
	require.ErrorContains(t, err, "linux/amd64")
	_, err = r.ResolveInstaller(context.Background(), "20260323-0_rc03", "arm64")
	require.NoError(t, err)
}

func TestInstallerCacheIsBoundedAndRedirectWithinOriginIsAllowed(t *testing.T) {
	r := NewResolver()
	requests := 0
	r.client.Transport = artifactTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		if strings.HasSuffix(req.URL.Path, "/redirected") {
			return artifactResponse(publicIndex(t), 200, dockerIndex), nil
		}
		response := artifactResponse("", 302, "")
		response.Header.Set("Location", "https://gke.gcr.io/redirected")
		return response, nil
	})
	for build := 0; build <= maxCacheEntries; build++ {
		_, err := r.ResolveInstaller(context.Background(), fmt.Sprintf("20260323-%d_rc03", build), "amd64")
		require.NoError(t, err)
	}
	before := requests
	_, err := r.ResolveInstaller(context.Background(), "20260323-0_rc03", "amd64")
	require.NoError(t, err)
	require.Equal(t, before+2, requests, "oldest positive resolution must be evicted from the bounded cache")
}
