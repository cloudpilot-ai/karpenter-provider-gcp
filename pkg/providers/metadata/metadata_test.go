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
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/yaml"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func applyMetadata(meta *compute.Metadata, patch func(InstanceMetadata) error) error {
	values := FromAPI(meta)
	original := cloneInstanceMetadata(values)
	if err := patch(values); err != nil {
		return err
	}
	if !testInstanceMetadataEqual(original, values) {
		ReplaceAPI(meta, values)
	}
	return nil
}

func mutateMetadata(meta *compute.Metadata, patch func(InstanceMetadata)) {
	_ = applyMetadata(meta, func(values InstanceMetadata) error {
		patch(values)
		return nil
	})
}

func testInstanceMetadataEqual(a, b InstanceMetadata) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestSetNodeLabelsDoesNotPartiallyApplyOnKubeEnvError(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeLabelsKey, Value: lo.ToPtr("existing=true")},
		{Key: KubeEnvKey, Value: lo.ToPtr("KUBELET_ARGS: --v=2\n")},
	}}

	err := applyMetadata(meta, func(values InstanceMetadata) error {
		return SetNodeLabels(values, map[string]string{"new-label": "true"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--node-labels flag not found")

	require.Equal(t, "existing=true", kubeLabelsValue(meta))
	require.NotContains(t, kubeLabelsValue(meta), "new-label=true")
}

func TestSetNodeLabelsOnlySyncsOwnedLabels(t *testing.T) {
	meta := gpuTestMeta("template-only=true,owned=old", "kubelet-only=true,owned=old")

	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error { return SetNodeLabels(values, map[string]string{"owned": "new"}) }))

	require.Equal(t, "owned=new,template-only=true", kubeLabelsValue(meta))
	require.Contains(t, kubeEnvValue(meta), "--node-labels=kubelet-only=true,owned=new")
	require.NotContains(t, kubeLabelsValue(meta), "kubelet-only=true")
	require.NotContains(t, kubeEnvValue(meta), "template-only=true")
}

func TestInstanceMetadataFromAPIToAPISortsKeys(t *testing.T) {
	api := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "z", Value: lo.ToPtr("last")},
		{Key: "a", Value: nil},
		{Key: "m", Value: lo.ToPtr("middle")},
	}}

	m := FromAPI(api)
	require.Equal(t, "", m["a"])
	require.Equal(t, "middle", m["m"])
	require.Equal(t, "last", m["z"])

	out := m.ToAPI()
	require.Len(t, out.Items, 3)
	require.Equal(t, []string{"a", "m", "z"}, []string{out.Items[0].Key, out.Items[1].Key, out.Items[2].Key})
	require.Equal(t, "", lo.FromPtr(out.Items[0].Value))
}

func TestInstanceMetadataFromAPIDuplicateLastWins(t *testing.T) {
	api := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "k", Value: lo.ToPtr("old")},
		{Key: "k", Value: lo.ToPtr("new")},
	}}

	require.Equal(t, "new", FromAPI(api)["k"])
}

func TestInstanceMetadataFromAPIIgnoresNilAndEmptyKeyItems(t *testing.T) {
	api := &compute.Metadata{Items: []*compute.MetadataItems{
		nil,
		{Key: "", Value: lo.ToPtr("ignored")},
		{Key: "k", Value: lo.ToPtr("value")},
	}}

	require.Empty(t, FromAPI(nil))
	require.Equal(t, InstanceMetadata{"k": "value"}, FromAPI(api))
}

func TestInstanceMetadataMergeUserSkipsEmptyAndReserved(t *testing.T) {
	m := InstanceMetadata{"existing": "template", KubeEnvKey: "system"}
	m.MergeUser(map[string]string{
		"existing": "user",
		"empty":    "",
		KubeEnvKey: "user-system",
		"new":      "value",
	}, map[string]struct{}{KubeEnvKey: {}})

	require.Equal(t, "user", m["existing"])
	require.Equal(t, "system", m[KubeEnvKey])
	require.Equal(t, "value", m["new"])
	require.NotContains(t, m, "empty")
}

func TestInstanceMetadataMergeUserNilReceiverNoPanic(t *testing.T) {
	var m InstanceMetadata
	require.NotPanics(t, func() {
		m.MergeUser(map[string]string{"new": "value"}, nil)
	})
	require.Nil(t, m)
}

func TestReplaceAPIPreservesFieldsAndReplacesSortedItems(t *testing.T) {
	target := &compute.Metadata{
		Fingerprint: "existing-fingerprint",
		Items: []*compute.MetadataItems{
			{Key: "old", Value: lo.ToPtr("value")},
		},
	}

	ReplaceAPI(target, InstanceMetadata{
		"z": "last",
		"a": "first",
	})

	require.Equal(t, "existing-fingerprint", target.Fingerprint)
	require.Len(t, target.Items, 2)
	require.Equal(t, "a", target.Items[0].Key)
	require.Equal(t, "first", lo.FromPtr(target.Items[0].Value))
	require.Equal(t, "z", target.Items[1].Key)
	require.Equal(t, "last", lo.FromPtr(target.Items[1].Value))
}

func TestKubeLabelsParseStringCanonicalizes(t *testing.T) {
	labels := ParseKubeLabels("b=2,,a=1,broken,c=3,")
	require.Equal(t, "1", labels.Get("a"))
	require.Equal(t, "2", labels.Get("b"))
	require.Equal(t, "a=1,b=2,c=3", labels.String())
}

func TestKubeLabelsSetDeleteMerge(t *testing.T) {
	labels := ParseKubeLabels("a=old,b=2")
	labels.Set("a", "new")
	labels.Set("", "ignored")
	labels.Set("empty", "")
	labels.Delete("b")
	labels.Merge(map[string]string{"c": "3", "merged-empty": "", "": "ignored"})

	require.Equal(t, "a=new,c=3,empty=,merged-empty=", labels.String())
}

func TestKubeLabelsSetNilReceiverNoPanic(t *testing.T) {
	var labels KubeLabels
	require.NotPanics(t, func() {
		labels.Set("empty", "")
		labels.Merge(map[string]string{"merged-empty": ""})
	})
	require.Nil(t, labels)
}

func TestKubeLabelsDuplicateLastWins(t *testing.T) {
	labels := ParseKubeLabels("a=old,a=new")
	require.Equal(t, "a=new", labels.String())
}

func TestKubeLabelsParseTrimsAndSkipsMalformedEntries(t *testing.T) {
	labels := ParseKubeLabels(" a = 1 , =missing-key , broken , b=2 , c= ")
	require.Equal(t, KubeLabels{"a": "1", "b": "2", "c": ""}, labels)
	require.Equal(t, "a=1,b=2,c=", labels.String())
}

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
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "amd64", "", nil)
	}))
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
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "arm64", "", client)
	}))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "linux-arm64.tar.gz")
	require.NotContains(t, got, "linux-amd64.tar.gz")
	require.Contains(t, got, fakeHash)
}

func TestPatchKubeEnvForArch_NoServerBinaryURL_NoOp(t *testing.T) {
	meta := kubeEnvMeta("KUBELET_ARGS: --v=2\n")
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "arm64", "", nil)
	}))
	require.Equal(t, "KUBELET_ARGS: --v=2\n", lo.FromPtr(meta.Items[0].Value))
}

func TestPatchKubeEnvForArch_VersionFallbackToGKEVersion(t *testing.T) {
	// URL has no /release/<version>/ segment; gkeVersion should be used instead.
	// Use a distinct version to avoid hitting the cache populated by TestPatchKubeEnvForArch_AMD64ToARM64.
	kubeEnv := "SERVER_BINARY_TAR_URL: https://example.com/kubernetes-server-linux-amd64.tar.gz\nSERVER_BINARY_TAR_HASH: " + strings.Repeat("0", 128) + "\n"
	srv := hashServer(t, fakeHash, http.StatusOK)
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "arm64", "v1.34.3-gke.9999999", client)
	}))
	require.Contains(t, lo.FromPtr(meta.Items[0].Value), "linux-arm64.tar.gz")
}

func TestPatchKubeEnvForArch_ARM64ToAMD64(t *testing.T) {
	arm64KubeEnv := strings.ReplaceAll(amd64KubeEnv, "amd64", "arm64")
	srv := hashServer(t, fakeHash, http.StatusOK)
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	meta := kubeEnvMeta(arm64KubeEnv)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "amd64", "", client)
	}))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "linux-amd64.tar.gz")
	require.NotContains(t, got, "linux-arm64.tar.gz")
	require.Contains(t, got, fakeHash)
}

func TestPatchKubeEnvForArch_NoVersionAnywhere_Error(t *testing.T) {
	kubeEnv := "SERVER_BINARY_TAR_URL: https://example.com/kubernetes-server-linux-amd64.tar.gz\n"
	meta := kubeEnvMeta(kubeEnv)
	err := applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "arm64", "", nil)
	})
	require.ErrorContains(t, err, "could not extract GKE release version")
}

func TestPatchKubeEnvForArch_HashFetchFails_Error(t *testing.T) {
	srv := hashServer(t, "", http.StatusInternalServerError)
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	// Use a distinct version so this test doesn't hit the cache populated by other tests.
	kubeEnv := "SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.99.0-gke.0000001/kubernetes-server-linux-amd64.tar.gz\n"
	err := applyMetadata(kubeEnvMeta(kubeEnv), func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "arm64", "", client)
	})
	require.Error(t, err)
}

// --- PatchKubeEnvForOSType ---

func TestPatchKubeEnvForOSType_COSToUbuntu(t *testing.T) {
	kubeEnv := "gke-os-distribution=cos\nENABLE_NODE_BFQ_IO_SCHEDULER: \"true\"\nNODE_BFQ_IO_SCHEDULER_IO_WEIGHT: \"1200\"\nKUBELET_ARGS: --v=2\n"
	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error { return PatchKubeEnvForOSType(values, v1alpha1.ImageFamilyUbuntu) }))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "gke-os-distribution=ubuntu")
	require.NotContains(t, got, "gke-os-distribution=cos")
	require.NotContains(t, got, "ENABLE_NODE_BFQ_IO_SCHEDULER")
	require.NotContains(t, got, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT")
}

func TestPatchKubeEnvForOSType_UbuntuToCOS(t *testing.T) {
	kubeEnv := "gke-os-distribution=ubuntu\nKUBELET_ARGS: --v=2\n"
	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForOSType(values, v1alpha1.ImageFamilyContainerOptimizedOS)
	}))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "gke-os-distribution=cos")
	require.NotContains(t, got, "gke-os-distribution=ubuntu")
	require.Contains(t, got, "ENABLE_NODE_BFQ_IO_SCHEDULER: \"true\"")
	require.Contains(t, got, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT: \"1200\"")
}

func TestPatchKubeEnvForOSType_COS_Idempotent(t *testing.T) {
	kubeEnv := "gke-os-distribution=ubuntu\nENABLE_NODE_BFQ_IO_SCHEDULER: \"true\"\nNODE_BFQ_IO_SCHEDULER_IO_WEIGHT: \"1200\"\nKUBELET_ARGS: --v=2\n"
	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForOSType(values, v1alpha1.ImageFamilyContainerOptimizedOS)
	}))
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForOSType(values, v1alpha1.ImageFamilyContainerOptimizedOS)
	}))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Equal(t, 1, strings.Count(got, "ENABLE_NODE_BFQ_IO_SCHEDULER"))
	require.Equal(t, 1, strings.Count(got, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT"))
}

func TestPatchKubeEnvForOSType_UnknownFamily_NoOp(t *testing.T) {
	kubeEnv := "gke-os-distribution=cos\nKUBELET_ARGS: --v=2\n"
	meta := kubeEnvMeta(kubeEnv)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error { return PatchKubeEnvForOSType(values, "CustomOS") }))
	require.Equal(t, kubeEnv, lo.FromPtr(meta.Items[0].Value))
}

func TestPatchKubeEnvForArch_MissingHashLine_Error(t *testing.T) {
	// kube-env has the binary URL but no SERVER_BINARY_TAR_HASH line.
	kubeEnv := "SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.0-gke.1/kubernetes-server-linux-amd64.tar.gz\n" +
		"KUBELET_ARGS: --v=2\n"
	srv := hashServer(t, fakeHash, http.StatusOK)
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	err := applyMetadata(kubeEnvMeta(kubeEnv), func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "arm64", "", client)
	})
	require.ErrorContains(t, err, "SERVER_BINARY_TAR_HASH not found")
}

// TestPatchKubeEnv_CrossOSAndArch verifies that applying both an OS-type patch and an
// arch patch in sequence produces the correct combined kube-env (COS/amd64 → Ubuntu/arm64).
func TestPatchKubeEnv_CrossOSAndArch(t *testing.T) {
	cosAMD64KubeEnv := "gke-os-distribution=cos\n" +
		"ENABLE_NODE_BFQ_IO_SCHEDULER: \"true\"\n" +
		"NODE_BFQ_IO_SCHEDULER_IO_WEIGHT: \"1200\"\n" +
		"SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.0-gke.2/kubernetes-server-linux-amd64.tar.gz\n" +
		"SERVER_BINARY_TAR_HASH: " + strings.Repeat("0", 128) + "\n" +
		"KUBELET_ARGS: --v=2\n"

	srv := hashServer(t, fakeHash, http.StatusOK)
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	meta := kubeEnvMeta(cosAMD64KubeEnv)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error { return PatchKubeEnvForOSType(values, v1alpha1.ImageFamilyUbuntu) }))
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "arm64", "", client)
	}))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "gke-os-distribution=ubuntu")
	require.NotContains(t, got, "gke-os-distribution=cos")
	require.NotContains(t, got, "ENABLE_NODE_BFQ_IO_SCHEDULER")
	require.Contains(t, got, "linux-arm64.tar.gz")
	require.NotContains(t, got, "linux-amd64.tar.gz")
	require.Contains(t, got, fakeHash)
}

// TestPatchKubeEnv_CrossOSAndArch_UbuntuCOSToARM64 verifies Ubuntu/amd64 → COS/arm64.
func TestPatchKubeEnv_CrossOSAndArch_UbuntuToCoSARM64(t *testing.T) {
	ubuntuAMD64KubeEnv := "gke-os-distribution=ubuntu\n" +
		"SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.0-gke.3/kubernetes-server-linux-amd64.tar.gz\n" +
		"SERVER_BINARY_TAR_HASH: " + strings.Repeat("0", 128) + "\n" +
		"KUBELET_ARGS: --v=2\n"

	srv := hashServer(t, fakeHash, http.StatusOK)
	defer srv.Close()
	client := srv.Client()
	client.Transport = rewriteHostTransport{base: http.DefaultTransport, target: srv.URL}

	meta := kubeEnvMeta(ubuntuAMD64KubeEnv)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForOSType(values, v1alpha1.ImageFamilyContainerOptimizedOS)
	}))
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return PatchKubeEnvForArch(context.Background(), values, "arm64", "", client)
	}))

	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, "gke-os-distribution=cos")
	require.NotContains(t, got, "gke-os-distribution=ubuntu")
	require.Contains(t, got, "ENABLE_NODE_BFQ_IO_SCHEDULER")
	require.Contains(t, got, "linux-arm64.tar.gz")
	require.NotContains(t, got, "linux-amd64.tar.gz")
	require.Contains(t, got, fakeHash)
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

func TestKubeletArgsParseStringMergesLabelsAndTaints(t *testing.T) {
	args := ParseKubeletArgs("--v=2 --max-pods=110 --node-labels=b=2,a=old --register-with-taints=t1:NoSchedule --node-labels=a=new --register-with-taints=t2:NoExecute,t1:NoSchedule --experimental-flag=value")
	args.MaxPods = "32"
	args.AddRegisterWithTaints("t3:NoSchedule")

	got := args.String()
	require.Equal(t, "--v=2 --experimental-flag=value --max-pods=32 --node-labels=a=new,b=2 --register-with-taints=t1:NoSchedule,t2:NoExecute,t3:NoSchedule", got)
	require.Equal(t, 1, strings.Count(got, "--max-pods="))
	require.Equal(t, 1, strings.Count(got, "--node-labels="))
	require.Equal(t, 1, strings.Count(got, "--register-with-taints="))
}

func TestKubeletArgsPreservesUnknownTokensWithFieldsLimitations(t *testing.T) {
	args := ParseKubeletArgs("before --node-labels=a=1 --quoted='two words' after")

	require.Equal(t, []string{"before", "--quoted='two", "words'", "after"}, args.Other)
	require.Equal(t, "before --quoted='two words' after --node-labels=a=1", args.String())
}

func TestKubeletArgsStringOmitsKnownFlagsWhenEmpty(t *testing.T) {
	args := ParseKubeletArgs("before --max-pods= --node-labels= --register-with-taints=,, after")

	require.Equal(t, "before after", args.String())
}

func TestPatchKubeletArgsLine(t *testing.T) {
	kubeEnv := "OTHER: value\nKUBELET_ARGS: --v=2 --node-labels=a=1\nTAIL: value\n"
	got, err := PatchKubeletArgsLine(kubeEnv, func(args *KubeletArgs) {
		args.NodeLabels.Set("b", "2")
		args.AddRegisterWithTaints("t1:NoSchedule")
	})
	require.NoError(t, err)
	require.Contains(t, got, "OTHER: value")
	require.Contains(t, got, "KUBELET_ARGS: --v=2 --node-labels=a=1,b=2 --register-with-taints=t1:NoSchedule")
	require.Contains(t, got, "TAIL: value")
}

func TestPatchKubeletArgsLineConsumesContinuationLines(t *testing.T) {
	kubeEnv := strings.Join([]string{
		"OTHER: value",
		"KUBELET_ARGS: --v=2",
		"  --node-labels=a=1",
		"  --volume-plugin-dir=/var/lib/kubelet/volumeplugins",
		"TAIL: value",
		"",
	}, "\n")

	got, err := PatchKubeletArgsLine(kubeEnv, func(args *KubeletArgs) {
		args.NodeLabels.Set("b", "2")
	})
	require.NoError(t, err)
	require.Contains(t, got, "OTHER: value")
	require.Contains(t, got, "TAIL: value")
	require.Contains(t, got, "--volume-plugin-dir=/var/lib/kubelet/volumeplugins")
	require.Equal(t, 1, strings.Count(got, "--node-labels="))
	require.Contains(t, got, "KUBELET_ARGS: --v=2 --volume-plugin-dir=/var/lib/kubelet/volumeplugins --node-labels=a=1,b=2")
	require.NotContains(t, got, "\n  --node-labels=")
	require.NotContains(t, got, "\n  --volume-plugin-dir=")
}

func TestPatchKubeletArgsLineErrorsWhenMissing(t *testing.T) {
	_, err := PatchKubeletArgsLine("OTHER: value\n", func(args *KubeletArgs) {})
	require.Error(t, err)
	require.Contains(t, err.Error(), "KUBELET_ARGS")
}

// kubeletConfigMeta returns a *compute.Metadata whose only entry is a
// kubelet-config item with the given YAML body. Mirrors a real GKE bootstrap
// template's kubelet-config metadata key.
func kubeletConfigMeta(initial string) *compute.Metadata {
	return &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: KubeletConfigLabel, Value: lo.ToPtr(initial)},
		},
	}
}

// instanceTypeWithKubeReserved returns a minimal *cloudprovider.InstanceType
// whose Overhead.KubeReserved holds the given cpu (milli), memory (Mi),
// and ephemeral-storage values. RenderKubeletConfigMetadata reads only these
// fields off the instance type.
func instanceTypeWithKubeReserved(cpuMilli, memMiB int64, ephemeralStorage string) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name: "test",
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved: corev1.ResourceList{
				corev1.ResourceCPU:              *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
				corev1.ResourceMemory:           *resource.NewQuantity(memMiB*1024*1024, resource.BinarySI),
				corev1.ResourceEphemeralStorage: resource.MustParse(ephemeralStorage),
			},
		},
	}
}

// renderedKubeletConfig invokes RenderKubeletConfigMetadata on a fresh
// baseKubeletConfigYAML and parses the resulting kubelet-config YAML back
// into a map so tests can assert on specific keys.
func renderedKubeletConfig(t *testing.T, nodeClass *v1alpha1.GCENodeClass, it *cloudprovider.InstanceType, capacityType string) map[string]interface{} {
	t.Helper()
	meta := kubeletConfigMeta(baseKubeletConfigYAML)
	require.NoError(t, applyMetadata(meta, func(values InstanceMetadata) error {
		return RenderKubeletConfigMetadata(values, nodeClass, it, capacityType)
	}))
	var got map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(lo.FromPtr(meta.Items[0].Value)), &got))
	return got
}

func nodeClassWith(kc *v1alpha1.KubeletConfiguration) *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{KubeletConfiguration: kc},
	}
}

const baseKubeletConfigYAML = "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\n"

func TestMergeUserKubeletConfig_NilLeavesConfigUntouched(t *testing.T) {
	config := map[string]interface{}{
		"kubeReserved": map[string]interface{}{
			"cpu":               "80m",
			"memory":            "100Mi",
			"ephemeral-storage": "15Gi",
		},
	}
	require.NoError(t, mergeUserKubeletConfig(config, nil))
	require.Equal(t, map[string]interface{}{
		"cpu":               "80m",
		"memory":            "100Mi",
		"ephemeral-storage": "15Gi",
	}, config["kubeReserved"])
}

func TestMergeUserKubeletConfig_EmptyStructLeavesConfigUntouched(t *testing.T) {
	config := map[string]interface{}{"kubeReserved": map[string]interface{}{"cpu": "80m"}}
	require.NoError(t, mergeUserKubeletConfig(config, &v1alpha1.KubeletConfiguration{}))
	require.Equal(t, map[string]interface{}{"cpu": "80m"}, config["kubeReserved"])
}

func TestMergeUserKubeletConfig_DeepMergesKubeReserved(t *testing.T) {
	// Provider-computed defaults already in config.
	config := map[string]interface{}{
		"kubeReserved": map[string]interface{}{
			"cpu":               "80m",
			"memory":            "100Mi",
			"ephemeral-storage": "15Gi",
		},
	}
	// User only sets cpu — memory and ephemeral-storage must survive.
	kc := &v1alpha1.KubeletConfiguration{
		KubeReserved: map[string]v1alpha1.KubeletQuantity{"cpu": "500m"},
	}
	require.NoError(t, mergeUserKubeletConfig(config, kc))
	got, ok := config["kubeReserved"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "500m", got["cpu"], "user cpu wins")
	require.Equal(t, "100Mi", got["memory"], "computed memory survives")
	require.Equal(t, "15Gi", got["ephemeral-storage"], "computed ephemeral-storage survives (issue #220 regression guard)")
}

func TestRenderKubeletConfigMetadata_NilKubeletConfiguration(t *testing.T) {
	got := renderedKubeletConfig(
		t,
		nodeClassWith(nil),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	// Only the provider-computed kubeReserved should appear; nothing else from a user struct.
	require.Equal(t, map[string]interface{}{
		"cpu":               "80m",
		"memory":            "100Mi",
		"ephemeral-storage": "15Gi",
	}, got["kubeReserved"])
	require.NotContains(t, got, "systemReserved")
	require.NotContains(t, got, "evictionHard")
}

func TestRenderKubeletConfigMetadata_EmptyKubeletConfiguration(t *testing.T) {
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	require.Equal(t, map[string]interface{}{
		"cpu":               "80m",
		"memory":            "100Mi",
		"ephemeral-storage": "15Gi",
	}, got["kubeReserved"])
}

func TestRenderKubeletConfigMetadata_SystemReserved(t *testing.T) {
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			SystemReserved: map[string]v1alpha1.KubeletQuantity{"cpu": "1", "memory": "1Gi"},
		}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	require.Equal(t, map[string]interface{}{"cpu": "1", "memory": "1Gi"}, got["systemReserved"])
}

func TestRenderKubeletConfigMetadata_KubeReservedUserWinsAndComputedSurvives(t *testing.T) {
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			KubeReserved: map[string]v1alpha1.KubeletQuantity{"cpu": "500m"},
		}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	kr, ok := got["kubeReserved"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "500m", kr["cpu"], "user cpu must win")
	require.Equal(t, "100Mi", kr["memory"], "computed memory must survive")
	require.Equal(t, "15Gi", kr["ephemeral-storage"], "computed ephemeral-storage must survive (issue #220 regression)")
}

func TestRenderKubeletConfigMetadata_KubeReservedSmallBootDiskIssue220Regression(t *testing.T) {
	// Simulate the issue #220 scenario: a small boot disk yields a small
	// computed ephemeral-storage reservation (e.g. 7Gi for a 30 GiB disk).
	// User sets cpu only. The small computed ephemeral-storage value must
	// survive — not be replaced by a larger default — otherwise
	// reservation > capacity and kubelet refuses to start.
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			KubeReserved: map[string]v1alpha1.KubeletQuantity{"cpu": "200m"},
		}),
		instanceTypeWithKubeReserved(60, 80, "7Gi"), // small disk → 7Gi computed reservation
		karpv1.CapacityTypeOnDemand,
	)
	kr := got["kubeReserved"].(map[string]interface{})
	require.Equal(t, "7Gi", kr["ephemeral-storage"], "computed small ephemeral-storage must survive user merge")
	require.Equal(t, "200m", kr["cpu"])
}

func TestRenderKubeletConfigMetadata_EvictionHard(t *testing.T) {
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			EvictionHard: map[string]v1alpha1.KubeletQuantity{"memory.available": "5%"},
		}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	require.Equal(t, map[string]interface{}{"memory.available": "5%"}, got["evictionHard"])
}

func TestRenderKubeletConfigMetadata_EvictionSoftGracePeriodDuration(t *testing.T) {
	// metav1.Duration.MarshalJSON renders as "30s" — verifies the
	// JSON-marshal round-trip handles Duration cleanly.
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			EvictionSoftGracePeriod: map[string]metav1.Duration{
				"memory.available": {Duration: 30 * time.Second},
			},
		}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	require.Equal(t, map[string]interface{}{"memory.available": "30s"}, got["evictionSoftGracePeriod"])
}

func TestRenderKubeletConfigMetadata_CPUCFSQuotaFalse(t *testing.T) {
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			CPUCFSQuota: lo.ToPtr(false),
		}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	v, ok := got["cpuCFSQuota"]
	require.True(t, ok, "cpuCFSQuota=false must appear in rendered YAML")
	require.Equal(t, false, v)
}

func TestRenderKubeletConfigMetadata_ClusterDNS(t *testing.T) {
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			ClusterDNS: []string{"10.0.1.100"},
		}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	v, ok := got["clusterDNS"].([]interface{})
	require.True(t, ok)
	require.Equal(t, []interface{}{"10.0.1.100"}, v)
}

func TestRenderKubeletConfigMetadata_ImageGCThresholds(t *testing.T) {
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			ImageGCHighThresholdPercent: lo.ToPtr(int32(85)),
			ImageGCLowThresholdPercent:  lo.ToPtr(int32(80)),
		}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeOnDemand,
	)
	// yaml.v3 unmarshals untyped numbers as int.
	require.EqualValues(t, 85, got["imageGCHighThresholdPercent"])
	require.EqualValues(t, 80, got["imageGCLowThresholdPercent"])
}

func TestRenderKubeletConfigMetadata_SpotAppliesAfterUserMerge(t *testing.T) {
	// Spot-specific kubelet keys (GracefulNodeShutdown feature gate and
	// the two shutdown grace periods) are applied AFTER the user merge,
	// so Spot preemption handling remains correct even when the user has
	// supplied an unrelated KubeletConfiguration. Forward-compatibility
	// guard: if any of those keys are ever added to v1alpha1.KubeletConfiguration,
	// the provider override still wins.
	got := renderedKubeletConfig(
		t,
		nodeClassWith(&v1alpha1.KubeletConfiguration{
			SystemReserved: map[string]v1alpha1.KubeletQuantity{"cpu": "1"},
		}),
		instanceTypeWithKubeReserved(80, 100, "15Gi"),
		karpv1.CapacityTypeSpot,
	)
	require.Equal(t, "30s", got["shutdownGracePeriod"])
	require.Equal(t, "15s", got["shutdownGracePeriodCriticalPods"])
	fg, ok := got["featureGates"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, true, fg["GracefulNodeShutdown"])
	// And the user value still landed (merge happened before Spot override).
	require.Equal(t, map[string]interface{}{"cpu": "1"}, got["systemReserved"])
}

func gpuTestMeta(kubeLabels, kubeEnvNodeLabels string) *compute.Metadata {
	return &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-labels", Value: lo.ToPtr(kubeLabels)},
			{
				Key:   "kube-env",
				Value: lo.ToPtr("KUBELET_ARGS: --v=2 --node-labels=" + kubeEnvNodeLabels + "\n"),
			},
		},
	}
}

func kubeLabelsValue(meta *compute.Metadata) string {
	for _, item := range meta.Items {
		if item.Key == "kube-labels" {
			return lo.FromPtr(item.Value)
		}
	}
	return ""
}

func kubeEnvValue(meta *compute.Metadata) string {
	return metadataValue(meta, "kube-env")
}

func metadataValue(meta *compute.Metadata, key string) string {
	for _, item := range meta.Items {
		if item.Key == key {
			return lo.FromPtr(item.Value)
		}
	}
	return ""
}
