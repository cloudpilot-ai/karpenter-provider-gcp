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
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
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

	got := lo.FromPtr(meta.Items[0].Value)
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
				Value: lo.ToPtr(`SERVER_BINARY_TAR_URL:
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

	got := lo.FromPtr(meta.Items[0].Value)
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
				Value: lo.ToPtr(`SERVER_BINARY_TAR_URL:
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

	got := lo.FromPtr(meta.Items[0].Value)
	// SERVER_BINARY_TAR_URL is left unchanged; the arch-native node pool template carries the correct URL and hash.
	require.Contains(t, got, "kubernetes-server-linux-arm64.tar.gz")
	require.Contains(t, got, "cloud.google.com/machine-family=e2")
	require.Contains(t, got, "arch=amd64")
	require.NotContains(t, got, "cloud.google.com/machine-family=c4a")
}

func TestPatchKubeEnvForInstanceType_SetsGKECPUScalingLevel(t *testing.T) {
	meta := gpuTestMeta(
		"cloud.google.com/machine-family=n2,cloud.google.com/gke-cpu-scaling-level=2",
		"cloud.google.com/machine-family=n2,cloud.google.com/gke-cpu-scaling-level=2",
	)
	it := &cloudprovider.InstanceType{
		Name: "g2-standard-4",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "g2"),
		),
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
		},
	}

	require.NoError(t, PatchKubeEnvForInstanceType(meta, it))

	for _, got := range []string{kubeLabelsValue(meta), kubeEnvValue(meta)} {
		require.Contains(t, got, "cloud.google.com/machine-family=g2")
		require.Contains(t, got, v1alpha1.LabelGKECPUScalingLevel+"=4")
		require.NotContains(t, got, v1alpha1.LabelGKECPUScalingLevel+"=2")
	}
}

func TestBaselineKubeEnvLabelsOmitsGKEReadinessLabels(t *testing.T) {
	// Readiness labels must NOT be hard-coded in the baseline; the correct set is
	// dataplane-specific and inherited from the source template in SetNodeLabels.
	labels := BaselineKubeEnvLabels(&karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		karpv1.NodePoolLabelKey: "default",
	}}}, &v1alpha1.GCENodeClass{})

	for _, key := range v1alpha1.GKEReadinessLabelKeys {
		require.NotContains(t, labels, key, "baseline must not hard-code readiness label %s", key)
	}
}

// TestSetNodeLabels_InheritsReadinessLabelsFromTemplate is the regression test for
// pod egress breaking on Dataplane V2 clusters: the source template's GKE readiness
// labels (here masq-agent-ds-ready, which gates ip-masq-agent and therefore SNAT)
// must survive into the provisioned node, while bootstrap pool labels are dropped.
func TestSetNodeLabels_InheritsReadinessLabelsFromTemplate(t *testing.T) {
	// A Dataplane V2 source template: has masq-agent + netd readiness, no kube-proxy.
	srcLabels := "cloud.google.com/gke-nodepool=bootstrap," +
		"node.kubernetes.io/masq-agent-ds-ready=true," +
		"cloud.google.com/gke-netd-ready=true," +
		"addon.gke.io/node-local-dns-ds-ready=true," +
		"iam.gke.io/gke-metadata-server-enabled=true"
	meta := gpuTestMeta(srcLabels, srcLabels)

	baseline := map[string]string{
		"max-pods-per-node":              "32",
		"karpenter.sh/nodepool":          "default",
		"karpenter.k8s.gcp/gcenodeclass": "default",
	}
	require.NoError(t, SetNodeLabels(meta, baseline, baseline))

	for _, got := range []string{kubeLabelsValue(meta), kubeEnvValue(meta)} {
		// Readiness labels inherited from the template.
		require.Contains(t, got, "node.kubernetes.io/masq-agent-ds-ready=true")
		require.Contains(t, got, "cloud.google.com/gke-netd-ready=true")
		require.Contains(t, got, "addon.gke.io/node-local-dns-ds-ready=true")
		require.Contains(t, got, "iam.gke.io/gke-metadata-server-enabled=true")
		// Karpenter-owned labels applied.
		require.Contains(t, got, "max-pods-per-node=32")
		require.Contains(t, got, "karpenter.sh/nodepool=default")
		// Bootstrap pool label dropped, and no kube-proxy readiness invented.
		require.NotContains(t, got, "cloud.google.com/gke-nodepool=bootstrap")
		require.NotContains(t, got, "node.kubernetes.io/kube-proxy-ds-ready")
	}
}

// TestSetNodeLabels_BaselineWinsOverInheritedReadiness verifies that when a key is
// both a readiness label in the template and explicitly owned by Karpenter, the
// Karpenter-owned value wins (no duplicate, no stale inherited value).
func TestSetNodeLabels_BaselineWinsOverInheritedReadiness(t *testing.T) {
	src := "iam.gke.io/gke-metadata-server-enabled=false,cloud.google.com/gke-nodepool=bootstrap"
	meta := gpuTestMeta(src, src)

	baseline := map[string]string{
		"karpenter.sh/nodepool":                  "default",
		"iam.gke.io/gke-metadata-server-enabled": "true",
	}
	require.NoError(t, SetNodeLabels(meta, baseline, baseline))

	for _, got := range []string{kubeLabelsValue(meta), kubeEnvValue(meta)} {
		require.Contains(t, got, "iam.gke.io/gke-metadata-server-enabled=true")
		require.NotContains(t, got, "iam.gke.io/gke-metadata-server-enabled=false")
		require.Equal(t, 1, strings.Count(got, "iam.gke.io/gke-metadata-server-enabled="))
	}
}

func TestAppendGPUTaint_MergesIntoExistingFlag(t *testing.T) {
	// The common case: PatchUnregisteredTaints has already added --register-with-taints.
	// AppendGPUTaint must merge into that value, not add a second flag.
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{
				Key:   "kube-env",
				Value: lo.ToPtr("KUBELET_ARGS: --v=2 --register-with-taints=karpenter.sh/unregistered=true:NoExecute\n"),
			},
		},
	}
	require.NoError(t, AppendGPUTaint(meta))
	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, GPUTaintArg)
	require.Contains(t, got, "karpenter.sh/unregistered=true:NoExecute")
	// Both taints must be in a single --register-with-taints flag, not two separate ones.
	require.Equal(t, 1, strings.Count(got, "--register-with-taints="), "must not introduce a second --register-with-taints flag")
}

func TestAppendGPUTaint_AppendsNewFlagWhenNoneExists(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{
				Key:   "kube-env",
				Value: lo.ToPtr("KUBELET_ARGS: --v=2\n"),
			},
		},
	}
	require.NoError(t, AppendGPUTaint(meta))
	got := lo.FromPtr(meta.Items[0].Value)
	require.Contains(t, got, GPUTaintArg)
}

func TestAppendGPUTaint_IdempotentWhenPresent(t *testing.T) {
	initial := "KUBELET_ARGS: --v=2 --register-with-taints=karpenter.sh/unregistered=true:NoExecute," + GPUTaintArg + "\n"
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{
				Key:   "kube-env",
				Value: lo.ToPtr(initial),
			},
		},
	}
	require.NoError(t, AppendGPUTaint(meta))
	got := lo.FromPtr(meta.Items[0].Value)
	require.Equal(t, 1, strings.Count(got, GPUTaintArg), "taint must appear exactly once")
}

func TestAppendGPUTaint_ErrorWhenNoKubeletArgs(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{
				Key:   "kube-env",
				Value: lo.ToPtr("SOME_OTHER_KEY: value\n"),
			},
		},
	}
	require.Error(t, AppendGPUTaint(meta))
}

func TestSetRegisterWithTaints_ReplacesSourceTaints(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{{
		Key:   "kube-env",
		Value: lo.ToPtr("KUBELET_ARGS: --v=2 --register-with-taints=dedicated=karpenter:NoSchedule,old=true:NoExecute\n"),
	}}}

	require.NoError(t, SetRegisterWithTaints(meta, []string{"karpenter.sh/unregistered=true:NoExecute"}))

	got := kubeEnvValue(meta)
	require.NotContains(t, got, "dedicated=karpenter:NoSchedule")
	require.NotContains(t, got, "old=true:NoExecute")
	require.Contains(t, got, "--register-with-taints=karpenter.sh/unregistered=true:NoExecute")
	require.Equal(t, 1, strings.Count(got, "--register-with-taints="))
}

func TestSetRegisterWithTaints_AddsFlagWhenMissing(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{{
		Key:   "kube-env",
		Value: lo.ToPtr("KUBELET_ARGS: --v=2\n"),
	}}}

	require.NoError(t, SetRegisterWithTaints(meta, []string{"karpenter.sh/unregistered=true:NoExecute"}))

	require.Contains(t, kubeEnvValue(meta), "--register-with-taints=karpenter.sh/unregistered=true:NoExecute")
}

func TestSetRegisterWithTaints_RemovesFlagWhenEmpty(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{{
		Key:   "kube-env",
		Value: lo.ToPtr("KUBELET_ARGS: --v=2 --register-with-taints=dedicated=karpenter:NoSchedule\n"),
	}}}

	require.NoError(t, SetRegisterWithTaints(meta, nil))

	got := kubeEnvValue(meta)
	require.NotContains(t, got, "--register-with-taints=")
	require.Contains(t, got, "KUBELET_ARGS: --v=2")
}

// gpuTestMeta returns a compute.Metadata that mirrors a real GKE template:
// kube-labels holds comma-separated node labels, and kube-env's KUBELET_ARGS carries
// those same labels in a --node-labels= flag (the canonical kubelet label source).
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
	for _, item := range meta.Items {
		if item.Key == "kube-env" {
			return lo.FromPtr(item.Value)
		}
	}
	return ""
}

func TestSetNodeLabels_ReplacesSourceLabelsInKubeLabelsAndKubeEnv(t *testing.T) {
	meta := gpuTestMeta(
		"cloud.google.com/gke-nodepool=bootstrap,workload=karpenter,max-pods-per-node=110",
		"cloud.google.com/gke-nodepool=bootstrap,workload=karpenter,max-pods-per-node=110",
	)
	kubeLabels := map[string]string{
		"max-pods-per-node":              "32",
		"karpenter.sh/nodepool":          "default",
		"karpenter.k8s.gcp/gcenodeclass": "default",
		"karpenter.sh/registered":        "true",
	}
	kubeEnvLabels := map[string]string{
		"max-pods-per-node":              "32",
		"karpenter.sh/nodepool":          "default",
		"karpenter.k8s.gcp/gcenodeclass": "default",
	}

	require.NoError(t, SetNodeLabels(meta, kubeLabels, kubeEnvLabels))

	for _, got := range []string{kubeLabelsValue(meta), kubeEnvValue(meta)} {
		require.NotContains(t, got, "cloud.google.com/gke-nodepool=bootstrap")
		require.NotContains(t, got, "workload=karpenter")
		require.Contains(t, got, "max-pods-per-node=32")
		require.Contains(t, got, "karpenter.sh/nodepool=default")
		require.Contains(t, got, "karpenter.k8s.gcp/gcenodeclass=default")
	}
	require.Contains(t, kubeLabelsValue(meta), "karpenter.sh/registered=true")
	require.NotContains(t, kubeEnvValue(meta), "karpenter.sh/registered=true")
	require.Equal(t, 1, strings.Count(kubeEnvValue(meta), "--node-labels="))
}

func TestSetNodeLabels_HandlesMissingNodeLabelsFlag(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("workload=karpenter")},
		{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2\n")},
	}}

	require.NoError(t, SetNodeLabels(meta, map[string]string{"karpenter.sh/nodepool": "default"}, map[string]string{"karpenter.sh/nodepool": "default"}))

	require.Equal(t, "karpenter.sh/nodepool=default", kubeLabelsValue(meta))
	require.Contains(t, kubeEnvValue(meta), "--node-labels=karpenter.sh/nodepool=default")
}

func TestSetDiskTypeLabelsReplacesInheritedLabels(t *testing.T) {
	meta := gpuTestMeta(
		"cloud.google.com/machine-family=n2,disk-type.gke.io/pd-balanced=true,disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np",
		"cloud.google.com/machine-family=n2,disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np",
	)

	require.NoError(t, SetDiskTypeLabels(meta, map[string]string{
		"disk-type.gke.io/hyperdisk-balanced": "true",
		"disk-type.gke.io/pd-ssd":             "true",
	}))

	require.Equal(t,
		"cloud.google.com/machine-family=n2,karpenter.sh/nodepool=np,disk-type.gke.io/hyperdisk-balanced=true,disk-type.gke.io/pd-ssd=true",
		kubeLabelsValue(meta),
	)
	require.Contains(t, kubeEnvValue(meta), "--node-labels=cloud.google.com/machine-family=n2,karpenter.sh/nodepool=np,disk-type.gke.io/hyperdisk-balanced=true,disk-type.gke.io/pd-ssd=true")
	require.NotContains(t, kubeEnvValue(meta), "disk-type.gke.io/pd-standard=true")
}

func TestSetDiskTypeLabelsHandlesMultilineKubeletArgs(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("cloud.google.com/machine-family=n2,disk-type.gke.io/pd-standard=true")},
		{Key: "kube-env", Value: lo.ToPtr(`KUBELET_ARGS: --v=2 --cloud-provider=external
  --cert-dir=/var/lib/kubelet/pki/ --node-labels=cloud.google.com/machine-family=n2,disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np
  --volume-plugin-dir=/home/kubernetes/flexvolume
`)},
	}}

	require.NoError(t, SetDiskTypeLabels(meta, map[string]string{
		"disk-type.gke.io/pd-balanced": "true",
	}))

	got := kubeEnvValue(meta)
	require.Contains(t, got, "--node-labels=cloud.google.com/machine-family=n2,karpenter.sh/nodepool=np,disk-type.gke.io/pd-balanced=true")
	require.Contains(t, got, "--volume-plugin-dir=/home/kubernetes/flexvolume")
	require.NotContains(t, got, "disk-type.gke.io/pd-standard=true")
}

func TestSetDiskTypeLabelsAllowsEmptySet(t *testing.T) {
	meta := gpuTestMeta(
		"disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np",
		"disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np",
	)

	require.NoError(t, SetDiskTypeLabels(meta, nil))

	require.Equal(t, "karpenter.sh/nodepool=np", kubeLabelsValue(meta))
	require.Contains(t, kubeEnvValue(meta), "--node-labels=karpenter.sh/nodepool=np")
	require.NotContains(t, kubeEnvValue(meta), "disk-type.gke.io/")
}

func TestSetGPUDriverVersionLabel_InjectsLabel(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, SetGPUDriverVersionLabel(meta, "latest"))
	require.Contains(t, kubeLabelsValue(meta), "cloud.google.com/gke-gpu-driver-version=latest")
	require.Contains(t, kubeEnvValue(meta), "cloud.google.com/gke-gpu-driver-version=latest")
}

func TestSetGPUDriverVersionLabel_ReplacesWhenLabelIsFirst(t *testing.T) {
	// GPU label is the first entry in --node-labels=. The strip regex eats only the
	// preceding comma, leaving "--node-labels=,next" without the fix.
	baseLabels := "cloud.google.com/gke-gpu-driver-version=default,max-pods-per-node=110"
	meta := gpuTestMeta(baseLabels, baseLabels)
	require.NoError(t, SetGPUDriverVersionLabel(meta, "latest"))
	ke := kubeEnvValue(meta)
	require.Contains(t, ke, "cloud.google.com/gke-gpu-driver-version=latest")
	require.NotContains(t, ke, "--node-labels=,", "leading comma must not appear in --node-labels after strip")
	require.Equal(t, 1, strings.Count(ke, "gke-gpu-driver-version="), "kube-env: must appear exactly once")
}

func TestSetGPUDriverVersionLabel_ReplacesExistingValue(t *testing.T) {
	baseLabels := "max-pods-per-node=110,cloud.google.com/gke-gpu-driver-version=default"
	meta := gpuTestMeta(baseLabels, baseLabels)
	require.NoError(t, SetGPUDriverVersionLabel(meta, "latest"))
	kl := kubeLabelsValue(meta)
	ke := kubeEnvValue(meta)
	// Old value replaced; new value present exactly once in both places.
	require.NotContains(t, kl, "gke-gpu-driver-version=default")
	require.Contains(t, kl, "cloud.google.com/gke-gpu-driver-version=latest")
	require.Equal(t, 1, strings.Count(kl, "gke-gpu-driver-version="), "kube-labels: must appear exactly once")
	require.NotContains(t, ke, "gke-gpu-driver-version=default")
	require.Contains(t, ke, "cloud.google.com/gke-gpu-driver-version=latest")
	require.Equal(t, 1, strings.Count(ke, "gke-gpu-driver-version="), "kube-env: must appear exactly once")
}

func TestSetGPUDriverVersionLabel_IdempotentOnRepeat(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, SetGPUDriverVersionLabel(meta, "default"))
	require.NoError(t, SetGPUDriverVersionLabel(meta, "default"))
	ke := kubeEnvValue(meta)
	require.Equal(t, 1, strings.Count(ke, "gke-gpu-driver-version="), "kube-env: label must appear exactly once")
}

func TestSetGPUDriverVersionLabel_ErrorWhenNoKubeLabels(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2 --node-labels=foo=bar\n")},
		},
	}
	err := SetGPUDriverVersionLabel(meta, "default")
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-labels")
}

func TestSetGPUDriverVersionLabel_ErrorWhenNoKubeEnv(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-labels", Value: lo.ToPtr("max-pods-per-node=110")},
		},
	}
	err := SetGPUDriverVersionLabel(meta, "default")
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-env")
}

func TestSetGPUAcceleratorLabel_InjectsLabel(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, SetGPUAcceleratorLabel(meta, "nvidia-tesla-t4"))
	require.Contains(t, kubeLabelsValue(meta), "cloud.google.com/gke-accelerator=nvidia-tesla-t4")
	require.Contains(t, kubeEnvValue(meta), "cloud.google.com/gke-accelerator=nvidia-tesla-t4")
}

func TestSetGPUAcceleratorLabel_ErrorWhenNoKubeLabels(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2 --node-labels=foo=bar\n")},
		},
	}
	err := SetGPUAcceleratorLabel(meta, "nvidia-tesla-a100")
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-labels")
}

func TestSetGPUAcceleratorLabel_ErrorWhenNoKubeEnv(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-labels", Value: lo.ToPtr("max-pods-per-node=110")},
		},
	}
	err := SetGPUAcceleratorLabel(meta, "nvidia-l4")
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-env")
}

func TestSetGPUAcceleratorLabel_ReplacesExistingValue(t *testing.T) {
	baseLabels := "max-pods-per-node=110,cloud.google.com/gke-accelerator=nvidia-tesla-t4"
	meta := gpuTestMeta(baseLabels, baseLabels)
	require.NoError(t, SetGPUAcceleratorLabel(meta, "nvidia-l4"))
	kl := kubeLabelsValue(meta)
	ke := kubeEnvValue(meta)
	require.NotContains(t, kl, "gke-accelerator=nvidia-tesla-t4")
	require.Contains(t, kl, "cloud.google.com/gke-accelerator=nvidia-l4")
	require.Equal(t, 1, strings.Count(kl, "gke-accelerator="), "kube-labels: must appear exactly once")
	require.NotContains(t, ke, "gke-accelerator=nvidia-tesla-t4")
	require.Contains(t, ke, "cloud.google.com/gke-accelerator=nvidia-l4")
	require.Equal(t, 1, strings.Count(ke, "gke-accelerator="), "kube-env: must appear exactly once")
}

func TestSetGPUAcceleratorLabel_IdempotentOnRepeat(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, SetGPUAcceleratorLabel(meta, "nvidia-tesla-t4"))
	require.NoError(t, SetGPUAcceleratorLabel(meta, "nvidia-tesla-t4"))
	ke := kubeEnvValue(meta)
	require.Equal(t, 1, strings.Count(ke, "gke-accelerator="), "kube-env: accelerator label must appear exactly once")
}

// metadataValue returns the value of the metadata item with the given key, or "" if absent.
func metadataValue(meta *compute.Metadata, key string) string {
	for _, item := range meta.Items {
		if item.Key == key {
			return lo.FromPtr(item.Value)
		}
	}
	return ""
}

func TestApplyCustomMetadata_OverridesExistingKey(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "serial-port-logging-enable", Value: lo.ToPtr("true")},
		},
	}

	ApplyCustomMetadata(meta, map[string]string{"serial-port-logging-enable": "false"})

	require.Len(t, meta.Items, 1, "override must not add a duplicate item for an existing key")
	require.Equal(t, "false", metadataValue(meta, "serial-port-logging-enable"),
		"existing value must be overridden, not concatenated")
}

func TestApplyCustomMetadata_AddsNewKey(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "existing", Value: lo.ToPtr("keep")},
		},
	}

	ApplyCustomMetadata(meta, map[string]string{"new-key": "new-value"})

	require.Equal(t, "keep", metadataValue(meta, "existing"), "unrelated keys must be left intact")
	require.Equal(t, "new-value", metadataValue(meta, "new-key"), "new key must be appended")
}

func TestApplyCustomMetadata_SkipsEmptyValue(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "serial-port-logging-enable", Value: lo.ToPtr("true")},
		},
	}

	ApplyCustomMetadata(meta, map[string]string{"serial-port-logging-enable": ""})

	require.Len(t, meta.Items, 1, "empty value must not remove the existing key")
	require.Equal(t, "true", metadataValue(meta, "serial-port-logging-enable"),
		"empty value is ignored: it cannot clear a value inherited from the base template")
}

func TestSetProvisioningModel_AddsSpotAfterLabelRebuild(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("workload=karpenter")},
		{Key: "kube-env", Value: lo.ToPtr("AUTOSCALER_ENV_VARS: gke-provisioning=standard\nKUBELET_ARGS: --v=2 --node-labels=workload=karpenter\n")},
	}}

	require.NoError(t, SetNodeLabels(meta, map[string]string{"karpenter.sh/nodepool": "default"}, map[string]string{"karpenter.sh/nodepool": "default"}))
	require.NoError(t, SetProvisioningModel(meta, "spot"))

	require.Contains(t, kubeLabelsValue(meta), "gke-provisioning=spot")
	require.Contains(t, kubeEnvValue(meta), "gke-provisioning=spot")
	require.NotContains(t, kubeEnvValue(meta), "gke-provisioning=standard")
	require.Contains(t, kubeEnvValue(meta), "--node-labels=karpenter.sh/nodepool=default,gke-provisioning=spot")
	require.NotContains(t, kubeLabelsValue(meta), "workload=karpenter")
	require.NotContains(t, kubeEnvValue(meta), "workload=karpenter")
}

func TestSetNodeLabels_ComposesWithGPUAndDiskLabelHelpers(t *testing.T) {
	meta := gpuTestMeta("workload=karpenter", "workload=karpenter")

	require.NoError(t, SetNodeLabels(meta, map[string]string{"karpenter.sh/nodepool": "default"}, map[string]string{"karpenter.sh/nodepool": "default"}))
	require.NoError(t, SetGPUAcceleratorLabel(meta, "nvidia-l4"))
	require.NoError(t, SetGPUDriverVersionLabel(meta, "latest"))
	require.NoError(t, SetDiskTypeLabels(meta, map[string]string{"disk-type.gke.io/pd-balanced": "true"}))

	for _, got := range []string{kubeLabelsValue(meta), kubeEnvValue(meta)} {
		require.NotContains(t, got, "workload=karpenter")
		require.Contains(t, got, "karpenter.sh/nodepool=default")
		require.Contains(t, got, "cloud.google.com/gke-accelerator=nvidia-l4")
		require.Contains(t, got, "cloud.google.com/gke-gpu-driver-version=latest")
		require.Contains(t, got, "disk-type.gke.io/pd-balanced=true")
	}
}

func TestSetRegisterWithTaints_ComposesWithAppendGPUTaint(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{{
		Key:   "kube-env",
		Value: lo.ToPtr("KUBELET_ARGS: --v=2 --register-with-taints=dedicated=karpenter:NoSchedule\n"),
	}}}

	require.NoError(t, SetRegisterWithTaints(meta, []string{"karpenter.sh/unregistered=true:NoExecute"}))
	require.NoError(t, AppendGPUTaint(meta))

	got := kubeEnvValue(meta)
	require.NotContains(t, got, "dedicated=karpenter:NoSchedule")
	require.Contains(t, got, "karpenter.sh/unregistered=true:NoExecute")
	require.Contains(t, got, GPUTaintArg)
	require.Equal(t, 1, strings.Count(got, "--register-with-taints="))
}
