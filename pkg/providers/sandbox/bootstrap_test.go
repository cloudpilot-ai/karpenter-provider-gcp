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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const manifestsURL = "https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.6-gke.1250001/kubernetes-manifests.tar.gz"
const serverURL = "https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.6-gke.1250001/kubernetes-server-linux-amd64.tar.gz"

func TestResolveBootstrapReviewedConsumersAndIdentity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/bootstrap-consumers.tar.gz")
	require.NoError(t, err)
	r := NewResolver()
	requests := 0
	r.client.Transport = artifactTransport(func(req *http.Request) (*http.Response, error) {
		require.True(t, req.URL.String() == manifestsURL || req.URL.String() == strings.Replace(manifestsURL, "1.35.6-gke.1250001", "1.35.7-gke.1250002", 1))
		requests++
		return artifactResponse(string(fixture), 200, "application/gzip"), nil
	})
	got, err := r.ResolveBootstrap(context.Background(), manifestsURL, serverURL)
	require.NoError(t, err)
	require.Equal(t, "v1.35.6-gke.1250001", got.Release)
	require.Equal(t, "gke-gvisor-20260323/native-xemu-smt-nftables-v1/amd64-core-tags", got.Identity("amd64-core-tags"))
	require.NotEqual(t, got.Identity("amd64-core-tags"), got.Identity("arm64"))
	require.Regexp(t, `^sha256:[a-f0-9]{64}$`, got.ArchiveDigest)
	_, err = r.ResolveBootstrap(context.Background(), manifestsURL, strings.Replace(serverURL, "amd64", "arm64", 1))
	require.NoError(t, err)
	require.Equal(t, 1, requests)
	// A new release is accepted only because all complete reviewed consumers
	// still match, not because its semver falls in a permitted range.
	next, err := r.ResolveBootstrap(context.Background(), strings.Replace(manifestsURL, "1.35.6-gke.1250001", "1.35.7-gke.1250002", 1), strings.Replace(serverURL, "1.35.6-gke.1250001", "1.35.7-gke.1250002", 1))
	require.NoError(t, err)
	require.Equal(t, "v1.35.7-gke.1250002", next.Release)
	require.Equal(t, 2, requests)

}

func TestResolveBootstrapRejectsUntrustedOrMismatchedSource(t *testing.T) {
	r := NewResolver()
	r.client.Transport = artifactTransport(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("invalid source must not fetch")
		return nil, nil
	})
	for _, urls := range [][2]string{
		{strings.Replace(manifestsURL, "https:", "http:", 1), serverURL},
		{strings.Replace(manifestsURL, "storage.googleapis.com", "example.org", 1), serverURL},
		{manifestsURL + "?token=anything", serverURL},
		{manifestsURL, strings.Replace(serverURL, "1250001", "1250002", 1)},
		{strings.Replace(manifestsURL, "v1.35.6-gke.1250001", "latest", 1), serverURL},
		{serverURL, serverURL},
	} {
		_, err := r.ResolveBootstrap(context.Background(), urls[0], urls[1])
		require.True(t, IsBootstrapUnsupported(err))
	}
}

func bootstrapArchive(t *testing.T, headers []*tar.Header, contents []string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for i, h := range headers {
		require.NoError(t, tw.WriteHeader(h))
		_, err := tw.Write([]byte(contents[i]))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return b.Bytes()
}
func TestResolveBootstrapFailsClosedOnUnknownConsumersAndBounds(t *testing.T) {
	changed := bootstrapArchive(t, []*tar.Header{{Name: "kubernetes/gci-trusty/installable.py", Size: 7, Mode: 0600}}, []string{"changed"})
	missing := bootstrapArchive(t, nil, nil)
	large := bootstrapArchive(t, []*tar.Header{{Name: "large", Size: maxMemberBytes + 1, Mode: 0600}}, []string{strings.Repeat("x", maxMemberBytes+1)})
	var bomb bytes.Buffer
	gz := gzip.NewWriter(&bomb)
	_, err := gz.Write([]byte(strings.Repeat("x", maxDecompressedBytes+1)))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	for name, body := range map[string][]byte{"changed consumer": changed, "missing consumers": missing, "member bound": large, "decompression bound": bomb.Bytes(), "compressed bound": bytes.Repeat([]byte("x"), maxCompressedBytes+1)} {
		t.Run(name, func(t *testing.T) {
			r := NewResolver()
			r.client.Transport = artifactTransport(func(_ *http.Request) (*http.Response, error) {
				return artifactResponse(string(body), 200, "application/gzip"), nil
			})
			_, err := r.ResolveBootstrap(context.Background(), manifestsURL, serverURL)
			require.True(t, IsBootstrapUnsupported(err), "%v", err)
		})
	}
}

func TestBootstrapArchiveRequiresUniqueRegularFullPathConsumers(t *testing.T) {
	headers, contents := bootstrapFixtureMembers(t)
	duplicated := bootstrapArchive(t, append(append([]*tar.Header{}, headers...), headers[0]), append(append([]string{}, contents...), contents[0]))
	require.ErrorContains(t, matchBootstrapArchive(duplicated), "duplicate")
	first := *headers[0]
	first.Name = "elsewhere/" + first.Name
	moved := append([]*tar.Header{&first}, headers[1:]...)
	require.ErrorContains(t, matchBootstrapArchive(bootstrapArchive(t, moved, contents)), "missing")
	first = *headers[0]
	first.Typeflag = tar.TypeSymlink
	first.Size = 0
	first.Linkname = "elsewhere"
	linked := append([]*tar.Header{&first}, headers[1:]...)
	linkedContents := append([]string{""}, contents[1:]...)
	require.ErrorContains(t, matchBootstrapArchive(bootstrapArchive(t, linked, linkedContents)), "non-regular")
}

func TestBootstrapRedirectCannotEscapePublicReleasePath(t *testing.T) {
	r := NewResolver()
	requests := 0
	r.client.Transport = artifactTransport(func(_ *http.Request) (*http.Response, error) {
		requests++
		response := artifactResponse("", 302, "")
		response.Header.Set("Location", "https://storage.googleapis.com/unrelated-bucket/kubernetes-manifests.tar.gz")
		return response, nil
	})
	_, err := r.ResolveBootstrap(context.Background(), manifestsURL, serverURL)
	require.True(t, IsBootstrapUnsupported(err))
	require.Equal(t, 1, requests)
}

func TestResolveBootstrapNativeQuotedMirrorsAndCacheIdentity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/bootstrap-consumers.tar.gz")
	require.NoError(t, err)
	r := NewResolver()
	var fetched []string
	r.client.Transport = artifactTransport(func(req *http.Request) (*http.Response, error) {
		fetched = append(fetched, req.URL.String())
		return artifactResponse(string(fixture), 200, "application/gzip"), nil
	})
	mirror := func(url string) string {
		return strings.Replace(url, "/gke-release/", "/gke-release-asia/", 1) + "," + url + "," + strings.Replace(url, "/gke-release/", "/gke-release-eu/", 1)
	}
	_, err = r.ResolveBootstrap(context.Background(), `"`+mirror(manifestsURL)+`"`, `'`+mirror(serverURL)+`'`)
	require.NoError(t, err)
	require.Equal(t, []string{strings.Replace(manifestsURL, "/gke-release/", "/gke-release-asia/", 1)}, fetched)
	_, err = r.ResolveBootstrap(context.Background(), mirror(manifestsURL), mirror(serverURL))
	require.NoError(t, err)
	require.Len(t, fetched, 1)
	// The same release on a different effective bucket is a different artifact.
	_, err = r.ResolveBootstrap(context.Background(), manifestsURL, serverURL)
	require.NoError(t, err)
	require.Len(t, fetched, 2)
}

func TestBootstrapMirrorValidationRejectsEveryUntrustedOrInconsistentEntry(t *testing.T) {
	for _, input := range [][2]string{
		{manifestsURL + ",https://example.org/bad", serverURL},
		{manifestsURL + "," + strings.Replace(manifestsURL, "1250001", "1250002", 1), serverURL},
		{manifestsURL, serverURL + "," + strings.Replace(serverURL, "amd64", "arm64", 1)},
		{manifestsURL + ",", serverURL},
		{`["` + manifestsURL + `"]`, serverURL},
		{`"` + manifestsURL + `" unexpected`, serverURL},
		{strings.Replace(manifestsURL, "gke-release/", "gke-release-untrusted/", 1), serverURL},
		{manifestsURL, serverURL + "#fragment"},
		{manifestsURL, strings.Replace(serverURL, "/release/", "/release/../release/", 1)},
	} {
		t.Run(input[0]+input[1], func(t *testing.T) {
			_, _, err := ValidateBootstrapURLs(input[0], input[1])
			require.True(t, IsBootstrapUnsupported(err), "%v", err)
		})
	}
}

func bootstrapFixtureMembers(t *testing.T) ([]*tar.Header, []string) {
	t.Helper()
	fixture, err := os.ReadFile("testdata/bootstrap-consumers.tar.gz")
	require.NoError(t, err)
	gz, err := gzip.NewReader(bytes.NewReader(fixture))
	require.NoError(t, err)
	defer gz.Close()
	tr := tar.NewReader(gz)
	var headers []*tar.Header
	var contents []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		headers = append(headers, h)
		contents = append(contents, string(b))
	}
	return headers, contents
}

func TestBootstrapArchiveRejectsAliasesLinksAndAncestorCollisions(t *testing.T) {
	headers, contents := bootstrapFixtureMembers(t)
	consumer := "kubernetes/gci-trusty/gci-configure-helper.sh"
	for _, tc := range []struct {
		name    string
		header  tar.Header
		content string
	}{
		{"dot alias", tar.Header{Name: "./" + consumer, Typeflag: tar.TypeReg}, "changed"},
		{"traversal alias", tar.Header{Name: "kubernetes/elsewhere/../gci-trusty/gci-configure-helper.sh", Typeflag: tar.TypeReg}, "changed"},
		{"absolute alias", tar.Header{Name: "/" + consumer, Typeflag: tar.TypeReg}, "changed"},
		{"symlink consumer", tar.Header{Name: consumer, Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}, ""},
		{"hardlink consumer", tar.Header{Name: consumer, Typeflag: tar.TypeLink, Linkname: "elsewhere"}, ""},
		{"symlink ancestor", tar.Header{Name: "kubernetes/gci-trusty", Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}, ""},
		{"non-directory ancestor", tar.Header{Name: "kubernetes", Typeflag: tar.TypeReg}, "changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.header
			h.Mode = 0600
			h.Size = int64(len(tc.content))
			for _, before := range []bool{false, true} {
				hs := append([]*tar.Header{}, headers...)
				bs := append([]string{}, contents...)
				if before {
					hs = append([]*tar.Header{&h}, hs...)
					bs = append([]string{tc.content}, bs...)
				} else {
					hs = append(hs, &h)
					bs = append(bs, tc.content)
				}
				require.Error(t, matchBootstrapArchive(bootstrapArchive(t, hs, bs)))
			}
		})
	}
}

func TestBootstrapOriginalReviewedPublicArchive(t *testing.T) {
	original, err := os.ReadFile("testdata/bootstrap-original.tar.gz")
	require.NoError(t, err)
	require.Equal(t, "sha256:"+acceptedBootstrap.ArchiveSHA256, digestBytes(original))
	require.NoError(t, matchBootstrapArchive(original))
}
