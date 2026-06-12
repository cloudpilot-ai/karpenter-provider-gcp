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
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestRemoveGKEBuiltinLabelsUsesCanonicalLabelsAndKeepsBroadKubeEnvRemoval(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeLabelsKey, Value: lo.ToPtr("b=2,cloud.google.com/gke-nodepool=pool-a,a=1")},
		{Key: KubeEnvKey, Value: lo.ToPtr("OTHER: cloud.google.com/gke-nodepool=pool-a\nKUBELET_ARGS: --v=2 --node-labels=b=2,cloud.google.com/gke-nodepool=pool-a,a=1\n")},
	}}

	require.NoError(t, applyMetadata(meta, RemoveGKEBuiltinLabels))

	require.Equal(t, "a=1,b=2", kubeLabelsValue(meta))
	require.Equal(t, "OTHER: \nKUBELET_ARGS: --v=2 --node-labels=a=1,b=2\n", metadataValue(meta, KubeEnvKey))
}

func TestRemoveGKEBuiltinLabelsRemovesNodePoolLabelRegardlessOfValue(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeLabelsKey, Value: lo.ToPtr("b=2,cloud.google.com/gke-nodepool=other-pool,a=1")},
		{Key: KubeEnvKey, Value: lo.ToPtr("OTHER: cloud.google.com/gke-nodepool=source-pool\nKUBELET_ARGS: --v=2 --node-labels=b=2,cloud.google.com/gke-nodepool=other-pool,a=1\n")},
	}}

	require.NoError(t, applyMetadata(meta, RemoveGKEBuiltinLabels))

	require.Equal(t, "a=1,b=2", kubeLabelsValue(meta))
	require.Equal(t, "OTHER: \nKUBELET_ARGS: --v=2 --node-labels=a=1,b=2\n", metadataValue(meta, KubeEnvKey))
}

func TestRemoveGKEBuiltinLabelsNoErrorWhenKeysOrKubeletArgsMissing(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeEnvKey, Value: lo.ToPtr("OTHER: cloud.google.com/gke-nodepool=pool-a\n")},
	}}

	require.NoError(t, applyMetadata(meta, RemoveGKEBuiltinLabels))
	require.Equal(t, "OTHER: \n", metadataValue(meta, KubeEnvKey))

	meta = &compute.Metadata{Items: []*compute.MetadataItems{{Key: "other", Value: lo.ToPtr("value")}}}
	require.NoError(t, applyMetadata(meta, RemoveGKEBuiltinLabels))
	require.Equal(t, "value", metadataValue(meta, "other"))
}

func TestAppendSecondaryBootDisksCanonicalizesLabelsAndPreservesKubeEnvAppend(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeLabelsKey, Value: lo.ToPtr("z=last")},
		{Key: KubeEnvKey, Value: lo.ToPtr("KUBELET_ARGS: --v=2\n")},
	}}
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{Disks: []v1alpha1.Disk{{
		SecondaryBootImage: "projects/project-a/global/images/cache-image",
		SecondaryBootMode:  "CONTAINER_IMAGE_CACHE",
	}}}}

	mutateMetadata(meta, func(values MetadataValues) { AppendSecondaryBootDisks("project-a", nodeClass, values) })

	require.Equal(t, "cloud.google.com.node-restriction.kubernetes.io/gke-secondary-boot-disk--cache-image=CONTAINER_IMAGE_CACHE.project-a,z=last", kubeLabelsValue(meta))
	require.Equal(t, "KUBELET_ARGS: --v=2\n\nSECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/gke-cache-image-disk", metadataValue(meta, KubeEnvKey))
}

func TestAppendSecondaryBootDisksNoOpsKubeLabelsWhenMissing(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{{Key: KubeEnvKey, Value: lo.ToPtr("KUBELET_ARGS: --v=2")}}}
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{Disks: []v1alpha1.Disk{{
		SecondaryBootImage: "global/images/cache-image",
		SecondaryBootMode:  "CONTAINER_IMAGE_CACHE",
	}}}}

	mutateMetadata(meta, func(values MetadataValues) { AppendSecondaryBootDisks("project-a", nodeClass, values) })

	require.Equal(t, "KUBELET_ARGS: --v=2\nSECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/gke-cache-image-disk", metadataValue(meta, KubeEnvKey))
}

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

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return PatchKubeEnvForInstanceType(values, it) }))

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

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return PatchKubeEnvForInstanceType(values, it) }))

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

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return PatchKubeEnvForInstanceType(values, it) }))

	got := lo.FromPtr(meta.Items[0].Value)
	// SERVER_BINARY_TAR_URL is left unchanged; the arch-native node pool template carries the correct URL and hash.
	require.Contains(t, got, "kubernetes-server-linux-arm64.tar.gz")
	require.Contains(t, got, "cloud.google.com/machine-family=e2")
	require.Contains(t, got, "arch=amd64")
	require.NotContains(t, got, "cloud.google.com/machine-family=c4a")
}

func TestPatchKubeEnvTaintsMergesExistingFlagsAndRequestedTaints(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "other", Value: lo.ToPtr("value")},
			{
				Key: KubeEnvKey,
				Value: lo.ToPtr(strings.Join([]string{
					"OTHER: value",
					"KUBELET_ARGS: --v=2 --register-with-taints=existing-1=true:NoSchedule,existing-2=true:NoExecute",
					"  --node-labels=a=1",
					"  --register-with-taints=existing-2=true:NoExecute,existing-3=true:NoSchedule",
					"TAIL: value",
					"",
				}, "\n")),
			},
		},
	}

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return PatchKubeEnvTaints(values, []string{"requested=true:NoSchedule", "existing-1=true:NoSchedule", "requested=true:NoSchedule"})
	}))
	got := FromAPI(meta)[KubeEnvKey]
	require.Contains(t, got, "KUBELET_ARGS: --v=2 --node-labels=a=1 --register-with-taints=existing-1=true:NoSchedule,existing-2=true:NoExecute,existing-3=true:NoSchedule,requested=true:NoSchedule")
	require.Contains(t, got, "OTHER: value")
	require.Contains(t, got, "TAIL: value")
	require.Equal(t, 1, strings.Count(got, "--register-with-taints="))
	require.NotContains(t, got, "\n  --register-with-taints=")
}

func TestPatchKubeEnvTaintsNoopsForEmptyTaints(t *testing.T) {
	meta := &compute.Metadata{}
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return PatchKubeEnvTaints(values, nil) }))
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return PatchKubeEnvTaints(values, []string{"", "  "}) }))
	require.Empty(t, meta.Items)
}

func TestPatchKubeEnvTaintsErrorsWhenKubeEnvMissing(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{{Key: "other", Value: lo.ToPtr("value")}}}
	err := applyMetadata(meta, func(values MetadataValues) error {
		return PatchKubeEnvTaints(values, []string{"requested=true:NoSchedule"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-env metadata not found")
}

func TestPatchKubeEnvTaintsErrorsWhenKubeletArgsMissing(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{{Key: KubeEnvKey, Value: lo.ToPtr("OTHER: value\n")}}}
	err := applyMetadata(meta, func(values MetadataValues) error {
		return PatchKubeEnvTaints(values, []string{"requested=true:NoSchedule"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "KUBELET_ARGS")
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
	require.NoError(t, applyMetadata(meta, AppendGPUTaint))
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
	require.NoError(t, applyMetadata(meta, AppendGPUTaint))
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
	require.NoError(t, applyMetadata(meta, AppendGPUTaint))
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
	require.Error(t, applyMetadata(meta, AppendGPUTaint))
}

// gpuTestMeta returns a compute.Metadata that mirrors a real GKE template:
// kube-labels holds comma-separated node labels, and kube-env's KUBELET_ARGS carries
// those same labels in a --node-labels= flag (the canonical kubelet label source).

func TestReplaceDelimitedAssignmentRewritesOnlyDelimitedKeys(t *testing.T) {
	got := replaceDelimitedAssignment(
		"arch=old,myarch=amd64 xarch=amd64;arch=amd64\tarch=old\npath=/x/arch=amd64",
		"arch",
		"arm64",
	)

	require.Equal(t, "arch=arm64,myarch=amd64 xarch=amd64;arch=arm64\tarch=arm64\npath=/x/arch=amd64", got)
}

func TestReplaceDelimitedAssignmentDoesNotRewriteSubstringKeys(t *testing.T) {
	got := replaceDelimitedAssignment(
		"not-max-pods=110,prefixmax-pods=110,max-pods-extra=110 max-pods=110",
		"max-pods",
		"32",
	)

	require.Equal(t, "not-max-pods=110,prefixmax-pods=110,max-pods-extra=110 max-pods=32", got)
}

func TestSetMaxPodsUpdatesKubeLabelsKubeletNodeLabelsAndKubeletMaxPods(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110,max-pods=110,other=label", "max-pods-per-node=110,max-pods=110,other=label")
	meta.Items[1].Value = lo.ToPtr("KUBELET_ARGS: --v=2 --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110,other=label\n")
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{KubeletConfiguration: &v1alpha1.KubeletConfiguration{MaxPods: lo.ToPtr(int32(32))}}}

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return SetMaxPods(values, nodeClass.GetMaxPods()) }))

	kl := kubeLabelsValue(meta)
	ke := kubeEnvValue(meta)
	require.Contains(t, kl, "max-pods-per-node=32")
	require.Contains(t, kl, "max-pods=32")
	require.Contains(t, kl, "other=label")
	require.NotContains(t, kl, "max-pods-per-node=110")
	require.NotContains(t, kl, "max-pods=110")
	require.Equal(t, 1, strings.Count(kl, "max-pods-per-node="))
	require.Equal(t, 1, strings.Count(kl, "max-pods="))
	require.Contains(t, ke, "max-pods-per-node=32")
	require.Contains(t, ke, "max-pods=32")
	require.Contains(t, ke, "other=label")
	require.NotContains(t, ke, "max-pods-per-node=110")
	require.NotContains(t, ke, "max-pods=110")
	require.NotContains(t, ke, "--max-pods=110")
	require.Equal(t, 1, strings.Count(ke, "max-pods-per-node="))
	require.Equal(t, 1, strings.Count(ke, "--max-pods="))
	require.Equal(t, 1, strings.Count(ke, "--node-labels="))
	require.Contains(t, ke, "--max-pods=32")
}

func TestSetMaxPodsPreservesNonKubeletArgsReplacements(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("max-pods-per-node=110,max-pods=110")},
		{Key: "kube-env", Value: lo.ToPtr("OTHER: max-pods-per-node=110,max-pods=110\nKUBELET_ARGS: --v=2 --node-labels=max-pods-per-node=110,max-pods=110\n")},
	}}

	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{KubeletConfiguration: &v1alpha1.KubeletConfiguration{MaxPods: lo.ToPtr(int32(32))}}}
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return SetMaxPods(values, nodeClass.GetMaxPods()) }))

	ke := kubeEnvValue(meta)
	require.Contains(t, ke, "OTHER: max-pods-per-node=32,max-pods=32")
	require.Contains(t, ke, "--node-labels=max-pods=32,max-pods-per-node=32")
}

func TestSetMaxPodsErrorsWhenKubeletArgsMissingNodeLabels(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("max-pods-per-node=110,max-pods=110")},
		{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2\n")},
	}}

	err := applyMetadata(meta, func(values MetadataValues) error {
		return SetMaxPods(values, (&v1alpha1.GCENodeClass{}).GetMaxPods())
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--node-labels flag not found")
}

func TestSetProvisioningModelUpdatesKubeLabelsAndKubeletNodeLabels(t *testing.T) {
	meta := gpuTestMeta("gke-provisioning=standard,max-pods=110", "gke-provisioning=standard,max-pods=110")

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return SetProvisioningModel(values, karpv1.CapacityTypeSpot) }))

	kl := kubeLabelsValue(meta)
	ke := kubeEnvValue(meta)
	require.Contains(t, kl, "gke-provisioning=spot")
	require.NotContains(t, kl, "gke-provisioning=standard")
	require.Equal(t, 1, strings.Count(kl, "gke-provisioning="))
	require.Contains(t, ke, "gke-provisioning=spot")
	require.NotContains(t, ke, "gke-provisioning=standard")
	require.Equal(t, 1, strings.Count(ke, "gke-provisioning="))
}

func TestSetProvisioningModelPreservesNonKubeletArgsReplacements(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("gke-provisioning=on-demand")},
		{Key: "kube-env", Value: lo.ToPtr("OTHER: gke-provisioning=on-demand\nKUBELET_ARGS: --v=2 --node-labels=gke-provisioning=on-demand\n")},
	}}

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error { return SetProvisioningModel(values, karpv1.CapacityTypeSpot) }))

	ke := kubeEnvValue(meta)
	require.Contains(t, ke, "OTHER: gke-provisioning=spot")
	require.Contains(t, ke, "--node-labels=gke-provisioning=spot")
	require.NotContains(t, ke, "gke-provisioning=on-demand")
}

func TestSetProvisioningModelErrorsWhenKubeletArgsMissingNodeLabels(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("gke-provisioning=standard")},
		{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2\n")},
	}}

	err := applyMetadata(meta, func(values MetadataValues) error { return SetProvisioningModel(values, karpv1.CapacityTypeSpot) })
	require.Error(t, err)
	require.Contains(t, err.Error(), "--node-labels flag not found")
}

func TestReplaceDiskTypeNodeLabelsReplacesInheritedLabels(t *testing.T) {
	meta := gpuTestMeta(
		"cloud.google.com/machine-family=n2,disk-type.gke.io/pd-balanced=true,disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np",
		"cloud.google.com/machine-family=n2,disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np",
	)

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return ReplaceNodeLabels(values, map[string]string{
			"disk-type.gke.io/hyperdisk-balanced": "true",
			"disk-type.gke.io/pd-ssd":             "true",
		}, func(key string) bool { return strings.HasPrefix(key, diskTypeLabelPrefix) })
	}))

	require.Equal(t,
		"cloud.google.com/machine-family=n2,disk-type.gke.io/hyperdisk-balanced=true,disk-type.gke.io/pd-ssd=true,karpenter.sh/nodepool=np",
		kubeLabelsValue(meta),
	)
	require.Contains(t, kubeEnvValue(meta), "--node-labels=cloud.google.com/machine-family=n2,disk-type.gke.io/hyperdisk-balanced=true,disk-type.gke.io/pd-ssd=true,karpenter.sh/nodepool=np")
	require.NotContains(t, kubeEnvValue(meta), "disk-type.gke.io/pd-standard=true")
}

func TestReplaceDiskTypeNodeLabelsHandlesMultilineKubeletArgs(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("cloud.google.com/machine-family=n2,disk-type.gke.io/pd-standard=true")},
		{Key: "kube-env", Value: lo.ToPtr(`KUBELET_ARGS: --v=2 --cloud-provider=external
  --cert-dir=/var/lib/kubelet/pki/ --node-labels=cloud.google.com/machine-family=n2,disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np
  --volume-plugin-dir=/home/kubernetes/flexvolume
`)},
	}}

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return ReplaceNodeLabels(values, map[string]string{
			"disk-type.gke.io/pd-balanced": "true",
		}, func(key string) bool { return strings.HasPrefix(key, diskTypeLabelPrefix) })
	}))

	got := kubeEnvValue(meta)
	require.Contains(t, got, "--node-labels=cloud.google.com/machine-family=n2,disk-type.gke.io/pd-balanced=true,karpenter.sh/nodepool=np")
	require.Contains(t, got, "--volume-plugin-dir=/home/kubernetes/flexvolume")
	require.NotContains(t, got, "disk-type.gke.io/pd-standard=true")
}

func TestReplaceDiskTypeNodeLabelsErrorsWhenKubeletArgsMissingNodeLabels(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("cloud.google.com/machine-family=n2,disk-type.gke.io/pd-standard=true")},
		{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2\n")},
	}}

	err := applyMetadata(meta, func(values MetadataValues) error {
		return ReplaceNodeLabels(values, map[string]string{"disk-type.gke.io/pd-balanced": "true"}, func(key string) bool { return strings.HasPrefix(key, diskTypeLabelPrefix) })
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--node-labels flag not found")
}

func TestReplaceDiskTypeNodeLabelsAllowsEmptySet(t *testing.T) {
	meta := gpuTestMeta(
		"disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np",
		"disk-type.gke.io/pd-standard=true,karpenter.sh/nodepool=np",
	)

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return ReplaceNodeLabels(values, nil, func(key string) bool { return strings.HasPrefix(key, diskTypeLabelPrefix) })
	}))

	require.Equal(t, "karpenter.sh/nodepool=np", kubeLabelsValue(meta))
	require.Contains(t, kubeEnvValue(meta), "--node-labels=karpenter.sh/nodepool=np")
	require.NotContains(t, kubeEnvValue(meta), "disk-type.gke.io/")
}

func TestSetGPUDriverVersionNodeLabel_InjectsLabel(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "latest"})
	}))
	require.Contains(t, kubeLabelsValue(meta), "cloud.google.com/gke-gpu-driver-version=latest")
	require.Contains(t, kubeEnvValue(meta), "cloud.google.com/gke-gpu-driver-version=latest")
}

func TestSetGPUDriverVersionNodeLabel_ReplacesWhenLabelIsFirst(t *testing.T) {
	// GPU label is the first entry in --node-labels=. The strip regex eats only the
	// preceding comma, leaving "--node-labels=,next" without the fix.
	baseLabels := "cloud.google.com/gke-gpu-driver-version=default,max-pods-per-node=110"
	meta := gpuTestMeta(baseLabels, baseLabels)
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "latest"})
	}))
	ke := kubeEnvValue(meta)
	require.Contains(t, ke, "cloud.google.com/gke-gpu-driver-version=latest")
	require.NotContains(t, ke, "--node-labels=,", "leading comma must not appear in --node-labels after strip")
	require.Equal(t, 1, strings.Count(ke, "gke-gpu-driver-version="), "kube-env: must appear exactly once")
}

func TestSetGPUDriverVersionNodeLabel_ReplacesExistingValue(t *testing.T) {
	baseLabels := "max-pods-per-node=110,cloud.google.com/gke-gpu-driver-version=default"
	meta := gpuTestMeta(baseLabels, baseLabels)
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "latest"})
	}))
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

func TestSetGPUDriverVersionNodeLabel_IdempotentOnRepeat(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "default"})
	}))
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "default"})
	}))
	ke := kubeEnvValue(meta)
	require.Equal(t, 1, strings.Count(ke, "gke-gpu-driver-version="), "kube-env: label must appear exactly once")
}

func TestSetGPUDriverVersionNodeLabel_ErrorWhenKubeletArgsMissingNodeLabels(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("max-pods-per-node=110")},
		{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2\n")},
	}}

	err := applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "default"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--node-labels flag not found")
}

func TestSetGPUDriverVersionNodeLabel_ErrorWhenNoKubeLabels(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2 --node-labels=foo=bar\n")},
		},
	}
	err := applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "default"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-labels")
}

func TestSetGPUDriverVersionNodeLabel_ErrorWhenNoKubeEnv(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-labels", Value: lo.ToPtr("max-pods-per-node=110")},
		},
	}
	err := applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "default"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-env")
}

func TestSetGPUAcceleratorNodeLabel_InjectsLabel(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-tesla-t4"})
	}))
	require.Contains(t, kubeLabelsValue(meta), "cloud.google.com/gke-accelerator=nvidia-tesla-t4")
	require.Contains(t, kubeEnvValue(meta), "cloud.google.com/gke-accelerator=nvidia-tesla-t4")
}

func TestSetGPUAcceleratorNodeLabel_ErrorWhenNoKubeLabels(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2 --node-labels=foo=bar\n")},
		},
	}
	err := applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-tesla-a100"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-labels")
}

func TestSetGPUAcceleratorNodeLabel_ErrorWhenKubeletArgsMissingNodeLabels(t *testing.T) {
	meta := &compute.Metadata{Items: []*compute.MetadataItems{
		{Key: "kube-labels", Value: lo.ToPtr("max-pods-per-node=110")},
		{Key: "kube-env", Value: lo.ToPtr("KUBELET_ARGS: --v=2\n")},
	}}

	err := applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-tesla-a100"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--node-labels flag not found")
}

func TestSetGPUAcceleratorNodeLabel_ErrorWhenNoKubeEnv(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "kube-labels", Value: lo.ToPtr("max-pods-per-node=110")},
		},
	}
	err := applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-l4"})
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kube-env")
}

func TestSetGPUAcceleratorNodeLabel_ReplacesExistingValue(t *testing.T) {
	baseLabels := "max-pods-per-node=110,cloud.google.com/gke-accelerator=nvidia-tesla-t4"
	meta := gpuTestMeta(baseLabels, baseLabels)
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-l4"})
	}))
	kl := kubeLabelsValue(meta)
	ke := kubeEnvValue(meta)
	require.NotContains(t, kl, "gke-accelerator=nvidia-tesla-t4")
	require.Contains(t, kl, "cloud.google.com/gke-accelerator=nvidia-l4")
	require.Equal(t, 1, strings.Count(kl, "gke-accelerator="), "kube-labels: must appear exactly once")
	require.NotContains(t, ke, "gke-accelerator=nvidia-tesla-t4")
	require.Contains(t, ke, "cloud.google.com/gke-accelerator=nvidia-l4")
	require.Equal(t, 1, strings.Count(ke, "gke-accelerator="), "kube-env: must appear exactly once")
}

func TestSetGPUAcceleratorNodeLabel_IdempotentOnRepeat(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-tesla-t4"})
	}))
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-tesla-t4"})
	}))
	ke := kubeEnvValue(meta)
	require.Equal(t, 1, strings.Count(ke, "gke-accelerator="), "kube-env: accelerator label must appear exactly once")
}

func TestSetGPULabelsNoCommaArtifacts(t *testing.T) {
	baseLabels := strings.Join([]string{
		"cloud.google.com/gke-accelerator=old",
		"cloud.google.com/gke-gpu-driver-version=old",
		"max-pods-per-node=110",
	}, ",")
	meta := gpuTestMeta(baseLabels, baseLabels)

	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-l4"})
	}))
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "latest"})
	}))
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEAccelerator: "nvidia-l4"})
	}))
	require.NoError(t, applyMetadata(meta, func(values MetadataValues) error {
		return SetNodeLabels(values, map[string]string{v1alpha1.LabelGKEGPUDriverVersion: "latest"})
	}))

	kl := kubeLabelsValue(meta)
	ke := kubeEnvValue(meta)
	for _, got := range []string{kl, ke} {
		require.NotContains(t, got, ",,")
		require.NotContains(t, got, "=,")
		require.NotContains(t, got, ", --")
	}
	require.False(t, strings.HasPrefix(kl, ","))
	require.False(t, strings.HasSuffix(kl, ","))
	require.NotContains(t, ke, "--node-labels=,")
	require.NotContains(t, ke, ", --register-with-taints=")
	require.Equal(t, 1, strings.Count(kl, "gke-accelerator="))
	require.Equal(t, 1, strings.Count(kl, "gke-gpu-driver-version="))
	require.Equal(t, 1, strings.Count(ke, "gke-accelerator="))
	require.Equal(t, 1, strings.Count(ke, "gke-gpu-driver-version="))
}

// metadataValue returns the value of the metadata item with the given key, or "" if absent.

func TestApplyCustomMetadata_OverridesExistingKey(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "serial-port-logging-enable", Value: lo.ToPtr("true")},
		},
	}

	mutateMetadata(meta, func(values MetadataValues) {
		ApplyCustomMetadata(values, map[string]string{"serial-port-logging-enable": "false"})
	})

	require.Len(t, meta.Items, 1, "override must not add a duplicate item for an existing key")
	require.Equal(t, "false", metadataValue(meta, "serial-port-logging-enable"),
		"existing value must be overridden, not concatenated")
}

func TestApplyCustomMetadata_AddsNewKeyAndSorts(t *testing.T) {
	meta := &compute.Metadata{
		Fingerprint: "existing-fingerprint",
		Items: []*compute.MetadataItems{
			{Key: "z", Value: lo.ToPtr("last")},
			{Key: "a", Value: lo.ToPtr("keep")},
		},
	}

	mutateMetadata(meta, func(values MetadataValues) { ApplyCustomMetadata(values, map[string]string{"new-key": "new-value"}) })

	require.Equal(t, "existing-fingerprint", meta.Fingerprint, "non-Items fields must be preserved")
	require.Equal(t, []string{"a", "new-key", "z"}, []string{meta.Items[0].Key, meta.Items[1].Key, meta.Items[2].Key})
	require.Equal(t, "keep", metadataValue(meta, "a"), "unrelated keys must be left intact")
	require.Equal(t, "new-value", metadataValue(meta, "new-key"), "new key must be added")
}

func TestApplyCustomMetadata_SkipsEmptyValue(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "z", Value: lo.ToPtr("last")},
			{Key: "serial-port-logging-enable", Value: lo.ToPtr("true")},
		},
	}

	mutateMetadata(meta, func(values MetadataValues) {
		ApplyCustomMetadata(values, map[string]string{"serial-port-logging-enable": ""})
	})

	require.Len(t, meta.Items, 2, "empty value must not remove the existing key")
	require.Equal(t, []string{"z", "serial-port-logging-enable"}, []string{meta.Items[0].Key, meta.Items[1].Key}, "empty value must not rewrite metadata")
	require.Equal(t, "true", metadataValue(meta, "serial-port-logging-enable"),
		"empty value is ignored: it cannot clear a value inherited from the base template")
}

func TestApplyCustomMetadata_EmptyCustomMapNoop(t *testing.T) {
	meta := &compute.Metadata{
		Items: []*compute.MetadataItems{
			{Key: "z", Value: lo.ToPtr("last")},
			{Key: "a", Value: lo.ToPtr("first")},
		},
	}

	mutateMetadata(meta, func(values MetadataValues) { ApplyCustomMetadata(values, nil) })

	require.Equal(t, []string{"z", "a"}, []string{meta.Items[0].Key, meta.Items[1].Key})
}
