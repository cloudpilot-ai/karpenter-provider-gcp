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

func TestSetGPUDriverVersionLabel_InjectsLabel(t *testing.T) {
	meta := gpuTestMeta("max-pods-per-node=110", "max-pods-per-node=110")
	require.NoError(t, SetGPUDriverVersionLabel(meta, "latest"))
	require.Contains(t, kubeLabelsValue(meta), "cloud.google.com/gke-gpu-driver-version=latest")
	require.Contains(t, kubeEnvValue(meta), "cloud.google.com/gke-gpu-driver-version=latest")
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
