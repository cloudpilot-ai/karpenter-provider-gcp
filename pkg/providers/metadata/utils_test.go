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

	"github.com/go-openapi/swag"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestNoPatchKubeEnv(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{},
		},
	}

	it := &cloudprovider.InstanceType{
		Name: "c4a-highmem-2",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
			scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "c4a"),
		),
	}

	require.NoError(t, PatchKubeEnvForInstanceType(meta, it))

	got := swag.StringValue(meta.Items[0].Value)
	require.NotContains(t, got, "kubernetes-server-linux-arm64.tar.gz")
	require.NotContains(t, got, "kubernetes-server-linux-amd64.tar.gz")
	require.NotContains(t, got, "cloud.google.com/machine-family=c4a")
	require.NotContains(t, got, "arch=arm64")
	require.NotContains(t, got, "cloud.google.com/machine-family=e2")
}

func TestPatchKubeEnvForInstanceType_ARM64(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{
				Key: "kube-env",
				Value: swag.String(`SERVER_BINARY_TAR_URL:
  https://storage.googleapis.com/gke-release-eu/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz,
  https://storage.googleapis.com/gke-release/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz,
  https://storage.googleapis.com/gke-release-asia/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz
AUTOSCALER_ENV_VARS: cloud.google.com/machine-family=e2, arch=amd64; something=else
KUBELET_ARGS: cloud.google.com/machine-family=e2, arch=amd64; --v=2
`),
			},
		},
	}

	it := &cloudprovider.InstanceType{
		Name: "c4a-highmem-2",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
			scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "c4a"),
		),
	}

	require.NoError(t, PatchKubeEnvForInstanceType(meta, it))

	got := swag.StringValue(meta.Items[0].Value)
	// SERVER_BINARY_TAR_URL is left unchanged; the arch-native node pool template carries the correct URL and hash.
	require.Contains(t, got, "kubernetes-server-linux-amd64.tar.gz")
	require.Contains(t, got, "cloud.google.com/machine-family=c4a")
	require.Contains(t, got, "arch=arm64")
	require.NotContains(t, got, "cloud.google.com/machine-family=e2")
}

func TestPatchKubeEnvForInstanceType_AMD64(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{
				Key: "kube-env",
				Value: swag.String(`SERVER_BINARY_TAR_URL:
  https://storage.googleapis.com/gke-release/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-arm64.tar.gz
AUTOSCALER_ENV_VARS: cloud.google.com/machine-family=c4a, arch=arm64;
KUBELET_ARGS: cloud.google.com/machine-family=c4a, arch=arm64;
`),
			},
		},
	}

	it := &cloudprovider.InstanceType{
		Name: "e2-standard-2",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "e2"),
		),
	}

	require.NoError(t, PatchKubeEnvForInstanceType(meta, it))

	got := swag.StringValue(meta.Items[0].Value)
	// SERVER_BINARY_TAR_URL is left unchanged by PatchKubeEnvForInstanceType;
	// arch patching is handled separately by PatchKubeEnvForArch.
	require.Contains(t, got, "kubernetes-server-linux-arm64.tar.gz")
	require.Contains(t, got, "cloud.google.com/machine-family=e2")
	require.Contains(t, got, "arch=amd64")
	require.NotContains(t, got, "cloud.google.com/machine-family=c4a")
}

// --- PatchKubeEnvForOSType tests ---

const cosKubeEnv = `AUTOSCALER_ENV_VARS: gke-os-distribution=cos,something=else
KUBELET_ARGS: --v=2 --node-labels=gke-os-distribution=cos
ENABLE_NODE_BFQ_IO_SCHEDULER: "true"
NODE_BFQ_IO_SCHEDULER_IO_WEIGHT: "1200"
OTHER_VAR: value
`

func TestPatchKubeEnvForOSType_COSIsNoop(t *testing.T) {
	meta := metaWithKubeEnv(cosKubeEnv)
	require.NoError(t, PatchKubeEnvForOSType(meta, v1alpha1.ImageFamilyContainerOptimizedOS))
	got := swag.StringValue(meta.Items[0].Value)
	require.Equal(t, cosKubeEnv, got, "COS input should be unchanged")
}

func TestPatchKubeEnvForOSType_Ubuntu(t *testing.T) {
	meta := metaWithKubeEnv(cosKubeEnv)
	require.NoError(t, PatchKubeEnvForOSType(meta, v1alpha1.ImageFamilyUbuntu))
	got := swag.StringValue(meta.Items[0].Value)

	require.Contains(t, got, "gke-os-distribution=ubuntu")
	require.NotContains(t, got, "gke-os-distribution=cos")
	require.NotContains(t, got, "ENABLE_NODE_BFQ_IO_SCHEDULER")
	require.NotContains(t, got, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT")
	require.Contains(t, got, "OTHER_VAR: value")
}

func TestPatchKubeEnvForOSType_EmptyKubeEnv(t *testing.T) {
	meta := metaWithKubeEnv("")
	err := PatchKubeEnvForOSType(meta, v1alpha1.ImageFamilyUbuntu)
	require.Error(t, err)
}

// --- PatchKubeEnvForArch tests ---

const amd64KubeEnv = `SERVER_BINARY_TAR_URL:
  https://storage.googleapis.com/gke-release-eu/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz,
  https://storage.googleapis.com/gke-release/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz,
  https://storage.googleapis.com/gke-release-asia/kubernetes/release/v1.34.3-gke.1444000/kubernetes-server-linux-amd64.tar.gz
SERVER_BINARY_TAR_HASH: aaaa%s
OTHER: val
`

func TestPatchKubeEnvForArch_SameArchIsNoop(t *testing.T) {
	kubeEnv := fmt.Sprintf(amd64KubeEnv, strings.Repeat("0", 124))
	meta := metaWithKubeEnv(kubeEnv)
	require.NoError(t, PatchKubeEnvForArch(context.Background(), meta, "amd64", "", http.DefaultClient))
	require.Equal(t, kubeEnv, swag.StringValue(meta.Items[0].Value), "same-arch should be no-op")
}

func TestPatchKubeEnvForArch_CrossArch(t *testing.T) {
	fakeHash := strings.Repeat("b", 128)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Must match the arm64 sha512 sidecar URL pattern.
		require.Contains(t, r.URL.Path, "linux-arm64.tar.gz.sha512")
		fmt.Fprint(w, fakeHash)
	}))
	defer srv.Close()

	// Override the GCS base URL via a custom http.Client that redirects to test server.
	// We can't override the URL in the function, so we'll verify the approach by
	// injecting a round-tripper that maps gke-release requests to the test server.
	transport := &rewriteTransport{base: http.DefaultTransport, from: "storage.googleapis.com", to: strings.TrimPrefix(srv.URL, "http://")}
	client := &http.Client{Transport: transport}

	kubeEnv := fmt.Sprintf(amd64KubeEnv, strings.Repeat("0", 124))
	// Invalidate the cache to ensure a fresh fetch.
	archHashCache.Delete("arm64:v1.34.3-gke.1444000")

	meta := metaWithKubeEnv(kubeEnv)
	require.NoError(t, PatchKubeEnvForArch(context.Background(), meta, "arm64", "", client))
	got := swag.StringValue(meta.Items[0].Value)

	require.NotContains(t, got, "linux-amd64.tar.gz")
	require.Contains(t, got, "linux-arm64.tar.gz")
	require.Contains(t, got, fakeHash)
}

func TestPatchKubeEnvForArch_CrossArch_WithExplicitVersion(t *testing.T) {
	fakeHash := strings.Repeat("d", 128)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "linux-arm64.tar.gz.sha512")
		fmt.Fprint(w, fakeHash)
	}))
	defer srv.Close()

	transport := &rewriteTransport{base: http.DefaultTransport, from: "storage.googleapis.com", to: strings.TrimPrefix(srv.URL, "http://")}
	client := &http.Client{Transport: transport}

	// Invalidate cache so a fresh fetch is required.
	archHashCache.Delete("arm64:v1.34.3-gke.1444000")

	kubeEnv := fmt.Sprintf(amd64KubeEnv, strings.Repeat("0", 124))
	meta := metaWithKubeEnv(kubeEnv)
	// Pass the version explicitly (GKE API source) — should not parse it from URL.
	require.NoError(t, PatchKubeEnvForArch(context.Background(), meta, "arm64", "v1.34.3-gke.1444000", client))
	got := swag.StringValue(meta.Items[0].Value)
	require.Contains(t, got, "linux-arm64.tar.gz")
	require.Contains(t, got, fakeHash)
}

func TestPatchKubeEnvForArch_CachesHash(t *testing.T) {
	fakeHash := strings.Repeat("c", 128)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, fakeHash)
	}))
	defer srv.Close()

	transport := &rewriteTransport{base: http.DefaultTransport, from: "storage.googleapis.com", to: strings.TrimPrefix(srv.URL, "http://")}
	client := &http.Client{Transport: transport}

	// Seed the cache to simulate a previous fetch.
	archHashCache.Store("arm64:v1.34.3-gke.1444000", fakeHash)

	kubeEnv := fmt.Sprintf(amd64KubeEnv, strings.Repeat("0", 124))
	meta := metaWithKubeEnv(kubeEnv)
	require.NoError(t, PatchKubeEnvForArch(context.Background(), meta, "arm64", "", client))
	require.Equal(t, 0, calls, "hash should be served from cache; no HTTP call expected")
}

// --- helpers ---

func metaWithKubeEnv(kubeEnv string) *compute.Metadata {
	return &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-env", Value: swag.String(kubeEnv)},
		},
	}
}

// rewriteTransport replaces the host in outbound requests so tests can redirect
// calls to storage.googleapis.com to a local httptest.Server.
type rewriteTransport struct {
	base http.RoundTripper
	from string
	to   string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if r.URL.Host == t.from {
		r.URL.Host = t.to
		r.URL.Scheme = "http"
	}
	return t.base.RoundTrip(r)
}
