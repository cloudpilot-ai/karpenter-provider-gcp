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

package instance

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

func TestValidateSourceKubeEnv(t *testing.T) {
	tests := []struct {
		name    string
		kubeEnv string
		want    string
	}{
		{
			name:    "complete",
			kubeEnv: requiredSourceKubeEnv + "KUBELET_ARGS: --v=2\n",
		},
		{
			name: "no kube-env",
			want: "missing required entries: CA_CERT, KUBE_MANIFESTS_TAR_URL, KUBERNETES_MASTER_NAME, SERVER_BINARY_TAR_HASH, SERVER_BINARY_TAR_URL",
		},
		{
			name:    "empty value",
			kubeEnv: strings.Replace(requiredSourceKubeEnv, "CA_CERT: test-ca", "CA_CERT: ", 1),
			want:    "missing required entries: CA_CERT",
		},
		{
			name:    "empty value with blank line",
			kubeEnv: strings.Replace(requiredSourceKubeEnv, "CA_CERT: test-ca\n", "CA_CERT: \n\n", 1),
			want:    "missing required entries: CA_CERT",
		},
		{
			name:    "quoted empty value",
			kubeEnv: strings.Replace(requiredSourceKubeEnv, "CA_CERT: test-ca", `CA_CERT: ""`, 1),
			want:    "missing required entries: CA_CERT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := &compute.Metadata{}
			if tt.kubeEnv != "" {
				source.Items = []*compute.MetadataItems{{Key: metadata.KubeEnvKey, Value: lo.ToPtr(tt.kubeEnv)}}
			}
			target, err := metadata.FromSourceTemplate(source)
			require.NoError(t, err)

			err = validateSourceKubeEnv(target)
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestApplyNodeClassKubeletConfigSetsImagePullParallelism(t *testing.T) {
	config := &kubeletconfig.KubeletConfiguration{}
	serialize := false
	maxParallel := int32(4)
	overlay := &v1alpha1.KubeletConfiguration{
		SerializeImagePulls:   &serialize,
		MaxParallelImagePulls: &maxParallel,
	}

	applyNodeClassKubeletConfig(config, overlay)

	require.NotNil(t, config.SerializeImagePulls)
	require.False(t, *config.SerializeImagePulls)
	require.NotNil(t, config.MaxParallelImagePulls)
	require.Equal(t, int32(4), *config.MaxParallelImagePulls)
}

func TestApplyNodeClassKubeletConfigLeavesImagePullParallelismUnsetWhenOverlayOmitsIt(t *testing.T) {
	config := &kubeletconfig.KubeletConfiguration{}
	maxPods := int32(64)
	overlay := &v1alpha1.KubeletConfiguration{MaxPods: &maxPods}

	applyNodeClassKubeletConfig(config, overlay)

	require.Nil(t, config.SerializeImagePulls)
	require.Nil(t, config.MaxParallelImagePulls)
	require.Equal(t, int32(64), config.MaxPods)
}

func TestBuildInstance_Hugepages2MMetadata(t *testing.T) {
	provider := makeProvider()
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LinuxNodeConfig: &v1alpha1.LinuxNodeConfig{
		Hugepages: &v1alpha1.HugepagesConfig{HugepageSize2m: lo.ToPtr[int32](4301)},
	}}}
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv + "HUGEPAGE_1G: \"16\"\nKUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110\n",
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-hugepages-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)

	require.NoError(t, err)
	kubeEnv := kubeEnvFrom(t, instance)
	require.Contains(t, kubeEnv, `HUGEPAGE_2M: "4301"`)
	require.Contains(t, kubeEnv, `ENABLE_CONTAINERD_HUGETLB_CONTROLLER: "true"`)
	require.NotContains(t, kubeEnv, "HUGEPAGE_1G")
}

func TestBuildInstance_Hugepages1GMetadata(t *testing.T) {
	provider := makeProvider()
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LinuxNodeConfig: &v1alpha1.LinuxNodeConfig{
		Hugepages: &v1alpha1.HugepagesConfig{HugepageSize1g: lo.ToPtr[int32](8)},
	}}}
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv + "HUGEPAGE_2M: \"256000\"\nKUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110\n",
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-hugepages-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)

	require.NoError(t, err)
	kubeEnv := kubeEnvFrom(t, instance)
	require.Contains(t, kubeEnv, `HUGEPAGE_1G: "8"`)
	require.Contains(t, kubeEnv, `ENABLE_CONTAINERD_HUGETLB_CONTROLLER: "true"`)
	require.NotContains(t, kubeEnv, "HUGEPAGE_2M")
}

func TestBuildInstance_HugepagesBothSizesMetadata(t *testing.T) {
	provider := makeProvider()
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LinuxNodeConfig: &v1alpha1.LinuxNodeConfig{
		Hugepages: &v1alpha1.HugepagesConfig{HugepageSize2m: lo.ToPtr[int32](512), HugepageSize1g: lo.ToPtr[int32](2)},
	}}}

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), makeSourceMetadata("max-pods-per-node=110,max-pods=110"),
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-hugepages-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)

	require.NoError(t, err)
	kubeEnv := kubeEnvFrom(t, instance)
	require.Contains(t, kubeEnv, `HUGEPAGE_2M: "512"`)
	require.Contains(t, kubeEnv, `HUGEPAGE_1G: "2"`)
	require.Contains(t, kubeEnv, `ENABLE_CONTAINERD_HUGETLB_CONTROLLER: "true"`)
}

func TestBuildInstance_DropsStaleSourceHugepagesMetadata(t *testing.T) {
	provider := makeProvider()
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv + "HUGEPAGE_2M: \"256000\"\nHUGEPAGE_1G: \"16\"\nKUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110\n",
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), &v1alpha1.GCENodeClass{}, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-hugepages-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)

	require.NoError(t, err)
	require.NotContains(t, kubeEnvFrom(t, instance), "HUGEPAGE_2M")
	require.NotContains(t, kubeEnvFrom(t, instance), "HUGEPAGE_1G")
}

func swapNodeClass(swap *v1alpha1.SwapConfig) *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{LinuxNodeConfig: &v1alpha1.LinuxNodeConfig{Swap: swap}}}
}

func buildSwapInstanceKubeEnv(t *testing.T, nodeClass *v1alpha1.GCENodeClass, sourceKubeEnv string) string {
	t.Helper()
	provider := makeProvider()
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv + sourceKubeEnv + "KUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110\n",
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-swap-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)
	require.NoError(t, err)
	return kubeEnvFrom(t, instance)
}

func TestBuildInstance_SwapBootDiskMetadata(t *testing.T) {
	kubeEnv := buildSwapInstanceKubeEnv(t, swapNodeClass(&v1alpha1.SwapConfig{
		Enabled:         true,
		BootDiskProfile: &v1alpha1.SwapBootDiskProfile{SwapSizeGib: 4},
	}), "")

	require.Contains(t, kubeEnv, "NODE_SWAP_PROFILE: BOOT_DISK")
	require.Contains(t, kubeEnv, "NODE_SWAP_SIZE: 4GiB")
	require.Contains(t, kubeEnv, `NODE_SWAP_ENCRYPTION: "true"`)
}

func TestBuildInstance_SwapEncryptionDisabledMetadata(t *testing.T) {
	kubeEnv := buildSwapInstanceKubeEnv(t, swapNodeClass(&v1alpha1.SwapConfig{
		Enabled:          true,
		EncryptionConfig: &v1alpha1.SwapEncryptionConfig{Disabled: lo.ToPtr(true)},
		BootDiskProfile:  &v1alpha1.SwapBootDiskProfile{SwapSizeGib: 1},
	}), "")

	require.Contains(t, kubeEnv, "NODE_SWAP_SIZE: 1GiB")
	require.Contains(t, kubeEnv, `NODE_SWAP_ENCRYPTION: "false"`)
}

func TestBuildInstance_DropsStaleSourceSwapMetadata(t *testing.T) {
	const sourceSwap = "NODE_SWAP_PROFILE: DEDICATED_LOCAL_SSD\nNODE_SWAP_SIZE: 100GiB\nNODE_SWAP_ENCRYPTION: \"true\"\n" +
		"NODE_SWAP_DEDICATED_LOCAL_SSD_COUNT: \"1\"\nNODE_ZSWAP_ENABLED: \"true\"\nNODE_ZSWAP_COMPRESSOR: zstd\nNODE_ZSWAP_MAX_POOL_PERCENT: \"20\"\n"

	for name, nodeClass := range map[string]*v1alpha1.GCENodeClass{
		"no linuxNodeConfig": {},
		"swap disabled":      swapNodeClass(&v1alpha1.SwapConfig{Enabled: false}),
	} {
		t.Run(name, func(t *testing.T) {
			kubeEnv := buildSwapInstanceKubeEnv(t, nodeClass, sourceSwap)
			require.NotContains(t, kubeEnv, "NODE_SWAP_")
			require.NotContains(t, kubeEnv, "NODE_ZSWAP_")
		})
	}
}

func TestBuildInstance_SwapReplacesSourceSwapMetadata(t *testing.T) {
	kubeEnv := buildSwapInstanceKubeEnv(t, swapNodeClass(&v1alpha1.SwapConfig{
		Enabled:         true,
		BootDiskProfile: &v1alpha1.SwapBootDiskProfile{SwapSizeGib: 2},
	}), "NODE_SWAP_PROFILE: DEDICATED_LOCAL_SSD\nNODE_SWAP_DEDICATED_LOCAL_SSD_COUNT: \"1\"\nNODE_ZSWAP_ENABLED: \"true\"\n")

	require.Contains(t, kubeEnv, "NODE_SWAP_PROFILE: BOOT_DISK")
	require.Contains(t, kubeEnv, "NODE_SWAP_SIZE: 2GiB")
	require.NotContains(t, kubeEnv, "DEDICATED_LOCAL_SSD")
	require.NotContains(t, kubeEnv, "NODE_ZSWAP_")
}

func timeSharingNodeClass(clients int32) *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{GPUSharingConfig: &v1alpha1.GPUSharingConfig{
		Strategy:               v1alpha1.GPUSharingStrategyTimeSharing,
		MaxSharedClientsPerGPU: clients,
	}}}
}

func TestBuildInstance_GPUTimeSharingMetadata(t *testing.T) {
	provider := makeProvider()

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), timeSharingNodeClass(4), makeGPUIT(), makeSourceMetadata("max-pods-per-node=110"),
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-gpu-sharing-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)

	require.NoError(t, err)
	kubeEnv := kubeEnvFrom(t, instance)
	require.Contains(t, kubeEnv, "GPU_SHARING_STRATEGY: time-sharing")
	require.Contains(t, kubeEnv, `MAX_SHARED_CLIENTS_PER_GPU: "4"`)
	require.Contains(t, kubeEnv, `MAX_TIME_SHARED_CLIENTS_PER_GPU: "4"`)
	for _, labels := range []string{kubeLabelsFrom(t, instance), kubeEnv} {
		require.Contains(t, labels, "cloud.google.com/gke-gpu-sharing-strategy=time-sharing")
		require.Contains(t, labels, "cloud.google.com/gke-max-shared-clients-per-gpu=4")
	}
}

func TestBuildInstance_GPUTimeSharingIgnoredForNonGPU(t *testing.T) {
	provider := makeProvider()

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), timeSharingNodeClass(4), makeNonGPUIT(), makeSourceMetadata("max-pods-per-node=110"),
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-gpu-sharing-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)

	require.NoError(t, err)
	kubeEnv := kubeEnvFrom(t, instance)
	require.NotContains(t, kubeEnv, "GPU_SHARING_STRATEGY")
	require.NotContains(t, kubeEnv, "SHARED_CLIENTS_PER_GPU")
	require.NotContains(t, kubeLabelsFrom(t, instance), "gke-gpu-sharing-strategy")
}

func TestBuildInstance_DropsStaleSourceGPUSharingMetadata(t *testing.T) {
	provider := makeProvider()
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv + "GPU_SHARING_STRATEGY: time-sharing\nMAX_SHARED_CLIENTS_PER_GPU: \"8\"\nMAX_TIME_SHARED_CLIENTS_PER_GPU: \"8\"\nKUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110\n",
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), &v1alpha1.GCENodeClass{}, makeGPUIT(), sourceMetadata,
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-gpu-sharing-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)

	require.NoError(t, err)
	kubeEnv := kubeEnvFrom(t, instance)
	require.NotContains(t, kubeEnv, "GPU_SHARING_STRATEGY")
	require.NotContains(t, kubeEnv, "SHARED_CLIENTS_PER_GPU")
}

func TestApplyNodeClassKubeletConfigSetsSwap(t *testing.T) {
	config := &kubeletconfig.KubeletConfiguration{FailSwapOn: lo.ToPtr(true)}
	overlay := &v1alpha1.KubeletConfiguration{
		FailSwapOn: lo.ToPtr(false),
		MemorySwap: &v1alpha1.MemorySwapConfiguration{SwapBehavior: "LimitedSwap"},
	}

	applyNodeClassKubeletConfig(config, overlay)

	require.NotNil(t, config.FailSwapOn)
	require.False(t, *config.FailSwapOn)
	require.Equal(t, "LimitedSwap", config.MemorySwap.SwapBehavior)
}

func TestApplyNodeClassKubeletConfigLeavesSwapUnsetWhenOverlayOmitsIt(t *testing.T) {
	config := &kubeletconfig.KubeletConfiguration{
		FailSwapOn: lo.ToPtr(true),
		MemorySwap: kubeletconfig.MemorySwapConfiguration{SwapBehavior: "NoSwap"},
	}
	overlay := &v1alpha1.KubeletConfiguration{
		MaxPods:    lo.ToPtr[int32](64),
		MemorySwap: &v1alpha1.MemorySwapConfiguration{},
	}

	applyNodeClassKubeletConfig(config, overlay)

	require.NotNil(t, config.FailSwapOn)
	require.True(t, *config.FailSwapOn, "the source template's failSwapOn must survive an overlay that omits it")
	require.Equal(t, "NoSwap", config.MemorySwap.SwapBehavior,
		"an empty memorySwap must not clear the source template's swapBehavior")
}

func TestBuildInstance_KubeletSwapConfig(t *testing.T) {
	provider := makeProvider()
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{KubeletConfiguration: &v1alpha1.KubeletConfiguration{
		FailSwapOn: lo.ToPtr(false),
		MemorySwap: &v1alpha1.MemorySwapConfiguration{SwapBehavior: "LimitedSwap"},
	}}}
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv + "KUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110\n",
		metadata.KubeletConfigKey: "failSwapOn: true\nnodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-swap-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)
	require.NoError(t, err)

	var got kubeletconfig.KubeletConfiguration
	require.NoError(t, yaml.Unmarshal([]byte(metadataValue(instance.Metadata, metadata.KubeletConfigKey)), &got))
	require.NotNil(t, got.FailSwapOn)
	require.False(t, *got.FailSwapOn, "the node class must override the source template's failSwapOn")
	require.Equal(t, "LimitedSwap", got.MemorySwap.SwapBehavior)
	require.Equal(t, "10s", got.NodeStatusUpdateFrequency.Duration.String(),
		"source kubelet-config entries the node class does not set must survive")
}

// The GCFSD_CACHE_SIZE_MIB values GKE 1.37 wrote into node pool templates.
func TestGCFSDCacheSizeMatchesGKENodePoolTemplates(t *testing.T) {
	for machineType, tc := range map[string]struct {
		memoryMiB int64
		want      int64
	}{
		"e2-standard-2":  {memoryMiB: 8 * 1024, want: 72},
		"e2-standard-4":  {memoryMiB: 16 * 1024, want: 104},
		"n2-standard-8":  {memoryMiB: 32 * 1024, want: 144},
		"c4-standard-16": {memoryMiB: 60 * 1024, want: 213},
		"n2-standard-16": {memoryMiB: 64 * 1024, want: 222},
	} {
		require.Equal(t, tc.want, gcfsdCacheSizeMiB(tc.memoryMiB), machineType)
	}
	require.Zero(t, gcfsdCacheSizeMiB(512), "GKE reserves nothing below 1 GiB")
}

func buildGCFSDInstanceKubeEnv(t *testing.T, sourceKubeEnv string, mt *computepb.MachineType) string {
	t.Helper()
	provider := makeProvider()
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv + sourceKubeEnv + "KUBELET_ARGS: --max-pods=110 --node-labels=max-pods-per-node=110,max-pods=110\n",
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), &v1alpha1.GCENodeClass{}, makeNonGPUIT(), sourceMetadata,
		makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false),
		"us-central1-a", "karpenter-gcfsd-test",
		karpv1.CapacityTypeOnDemand, mt, 0, "pods",
	)
	require.NoError(t, err)
	return kubeEnvFrom(t, instance)
}

func TestBuildInstance_SizesGCFSDCacheForMachineType(t *testing.T) {
	kubeEnv := buildGCFSDInstanceKubeEnv(t, "GCFSD_CACHE_SIZE_MIB: \"72\"\n", &computepb.MachineType{MemoryMb: lo.ToPtr[int32](60 * 1024)})

	require.Contains(t, kubeEnv, `GCFSD_CACHE_SIZE_MIB: "213"`)
}

func TestBuildInstance_GCFSDCacheAbsentWithoutImageStreaming(t *testing.T) {
	kubeEnv := buildGCFSDInstanceKubeEnv(t, "", &computepb.MachineType{MemoryMb: lo.ToPtr[int32](60 * 1024)})

	require.NotContains(t, kubeEnv, "GCFSD_CACHE_SIZE_MIB")
}

func TestBuildInstance_KeepsSourceGCFSDCacheWithoutMachineMemory(t *testing.T) {
	kubeEnv := buildGCFSDInstanceKubeEnv(t, "GCFSD_CACHE_SIZE_MIB: \"72\"\n", nil)

	require.Contains(t, kubeEnv, `GCFSD_CACHE_SIZE_MIB: "72"`)
}
