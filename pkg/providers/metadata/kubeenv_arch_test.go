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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// fakeHash is a valid-length (128-char) SHA-512 hex string used in tests.
const fakeHash = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2" +
	"c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"

func hashServer(t *testing.T, hash string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			fmt.Fprint(w, hash)
		}
	}))
}

func kubeEnvMeta(kubeEnv string) *compute.Metadata {
	return &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-env", Value: lo.ToPtr(kubeEnv)},
		},
	}
}

var amd64KubeEnv = "SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz\n" +
	"SERVER_BINARY_TAR_HASH: " + strings.Repeat("0", 128) + "\n" +
	"KUBELET_ARGS: --v=2\n"

// TestPatchKubeEnvForArch_SameArch_NoHTTP verifies no network call when source == target.
func TestPatchKubeEnvForArch_SameArch_NoHTTP(t *testing.T) {
	meta := kubeEnvMeta(amd64KubeEnv)
	// Passing a nil client would panic if a request were made.
	require.NoError(t, PatchKubeEnvForArch(context.Background(), meta, "amd64", "", nil))
	require.Contains(t, lo.FromPtr(meta.Items[0].Value), "linux-amd64.tar.gz")
}

func TestPatchKubeEnvForArch_AMD64ToARM64(t *testing.T) {
	srv := hashServer(t, fakeHash, http.StatusOK)
	defer srv.Close()
	client := srv.Client()
	// Override the URL in getArchHash by replacing the base — we use a custom client
	// pointing at the test server via transport, but getArchHash builds its own URL.
	// Inject via a RoundTripper that rewrites the host.
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	meta := kubeEnvMeta(amd64KubeEnv)
	require.NoError(t, PatchKubeEnvForArch(context.Background(), meta, "arm64", "", client))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "linux-arm64.tar.gz")
	require.NotContains(t, got, "linux-amd64.tar.gz")
	require.Contains(t, got, fakeHash)
}

func TestPatchKubeEnvForArch_NoServerBinaryURL_NoOp(t *testing.T) {
	meta := kubeEnvMeta("KUBELET_ARGS: --v=2\n")
	require.NoError(t, PatchKubeEnvForArch(context.Background(), meta, "arm64", "", nil))
	require.Equal(t, "KUBELET_ARGS: --v=2\n", lo.FromPtr(meta.Items[0].Value))
}

func TestPatchKubeEnvForArch_VersionFallbackToGKEVersion(t *testing.T) {
	// URL has no /release/<version>/ segment; gkeVersion should be used instead.
	kubeEnv := "SERVER_BINARY_TAR_URL: https://example.com/kubernetes-server-linux-amd64.tar.gz\nSERVER_BINARY_TAR_HASH: " + strings.Repeat("0", 128) + "\n"
	srv := hashServer(t, fakeHash, http.StatusOK)
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, PatchKubeEnvForArch(context.Background(), meta, "arm64", "v1.34.3-gke.1444000", client))
	require.Contains(t, lo.FromPtr(meta.Items[0].Value), "linux-arm64.tar.gz")
}

func TestPatchKubeEnvForArch_NoVersionAnywhere_Error(t *testing.T) {
	kubeEnv := "SERVER_BINARY_TAR_URL: https://example.com/kubernetes-server-linux-amd64.tar.gz\n"
	meta := kubeEnvMeta(kubeEnv)
	err := PatchKubeEnvForArch(context.Background(), meta, "arm64", "", nil)
	require.ErrorContains(t, err, "could not extract GKE release version")
}

func TestPatchKubeEnvForArch_HashFetchFails_Error(t *testing.T) {
	srv := hashServer(t, "", http.StatusInternalServerError)
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	// Use a distinct version so this test doesn't hit the cache populated by other tests.
	kubeEnv := "SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.99.0-gke.0000001/kubernetes-server-linux-amd64.tar.gz\n"
	err := PatchKubeEnvForArch(context.Background(), kubeEnvMeta(kubeEnv), "arm64", "", client)
	require.Error(t, err)
}

// --- PatchKubeEnvForOSType ---

func TestPatchKubeEnvForOSType_COSToUbuntu(t *testing.T) {
	kubeEnv := "gke-os-distribution=cos\nENABLE_NODE_BFQ_IO_SCHEDULER: \"true\"\nNODE_BFQ_IO_SCHEDULER_IO_WEIGHT: \"1200\"\nKUBELET_ARGS: --v=2\n"
	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, PatchKubeEnvForOSType(meta, v1alpha1.ImageFamilyUbuntu))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "gke-os-distribution=ubuntu")
	require.NotContains(t, got, "gke-os-distribution=cos")
	require.NotContains(t, got, "ENABLE_NODE_BFQ_IO_SCHEDULER")
	require.NotContains(t, got, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT")
}

func TestPatchKubeEnvForOSType_UbuntuToCOS(t *testing.T) {
	kubeEnv := "gke-os-distribution=ubuntu\nKUBELET_ARGS: --v=2\n"
	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, PatchKubeEnvForOSType(meta, v1alpha1.ImageFamilyContainerOptimizedOS))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "gke-os-distribution=cos")
	require.NotContains(t, got, "gke-os-distribution=ubuntu")
	require.Contains(t, got, "ENABLE_NODE_BFQ_IO_SCHEDULER: \"true\"")
	require.Contains(t, got, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT: \"1200\"")
}

func TestPatchKubeEnvForOSType_COS_Idempotent(t *testing.T) {
	kubeEnv := "gke-os-distribution=ubuntu\nENABLE_NODE_BFQ_IO_SCHEDULER: \"true\"\nNODE_BFQ_IO_SCHEDULER_IO_WEIGHT: \"1200\"\nKUBELET_ARGS: --v=2\n"
	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, PatchKubeEnvForOSType(meta, v1alpha1.ImageFamilyContainerOptimizedOS))
	require.NoError(t, PatchKubeEnvForOSType(meta, v1alpha1.ImageFamilyContainerOptimizedOS))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Equal(t, 1, strings.Count(got, "ENABLE_NODE_BFQ_IO_SCHEDULER"))
	require.Equal(t, 1, strings.Count(got, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT"))
}

func TestPatchKubeEnvForOSType_UnknownFamily_NoOp(t *testing.T) {
	kubeEnv := "gke-os-distribution=cos\nKUBELET_ARGS: --v=2\n"
	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, PatchKubeEnvForOSType(meta, "CustomOS"))
	require.Equal(t, kubeEnv, lo.FromPtr(meta.Items[0].Value))
}

// rewriteHostTransport rewrites all outgoing requests to point at the target server,
// allowing getArchHash (which builds its own GCS URL) to be tested without real network access.
type rewriteHostTransport struct {
	base   http.RoundTripper
	target string
}

func (r rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Host = strings.TrimPrefix(r.target, "http://")
	req.URL.Scheme = "http"
	return r.base.RoundTrip(req)
}
