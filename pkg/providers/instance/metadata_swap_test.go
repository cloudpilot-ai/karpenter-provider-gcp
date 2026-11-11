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
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

func swapMachineType() *computepb.MachineType {
	return &computepb.MachineType{Name: lo.ToPtr("n2-standard-4"), GuestCpus: lo.ToPtr[int32](4), MemoryMb: lo.ToPtr[int32](16384)}
}

// withSwapTestImages gives the node class the resolved image a boot disk needs.
func withSwapTestImages(nodeClass *v1alpha1.GCENodeClass) *v1alpha1.GCENodeClass {
	nodeClass.Status.Images = bootDiskNodeClass("pd-balanced", nil, nil).Status.Images
	return nodeClass
}

const (
	supportedManifestsURL = "https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.8-gke.1796000/kubernetes-manifests.tar.gz"
	oldManifestsURL       = "https://storage.googleapis.com/gke-release/kubernetes/release/v1.34.1-gke.999/kubernetes-manifests.tar.gz"
	newServerURL          = "https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.8-gke.1796000/kubernetes-server-linux-amd64.tar.gz"
	oldServerURL          = "https://storage.googleapis.com/gke-release/kubernetes/release/v1.30.1-gke.123/kubernetes-server-linux-amd64.tar.gz"
)

// TestBuildInstance_BootDiskSwapFollowsTheBootstrapManifestsArchive protects
// which archive decides support: the node bootstraps from
// KUBE_MANIFESTS_TAR_URL's helper, while SERVER_BINARY_TAR_URL only names the
// Kubernetes binaries to install, so the two releases can legitimately differ.
func TestBuildInstance_BootDiskSwapFollowsTheBootstrapManifestsArchive(t *testing.T) {
	for name, tc := range map[string]struct {
		manifestsURL string
		serverURL    string
		wantErr      string
	}{
		// GKE build numbers compare numerically: gke.999 is older than gke.1341000.
		"old manifests with a new server binary are unsupported": {
			manifestsURL: oldManifestsURL,
			serverURL:    newServerURL,
			wantErr:      "needs a GKE bootstrap of v1.34.1-gke.1341000 or newer",
		},
		"old Kubernetes version in the manifests": {
			manifestsURL: "https://storage.googleapis.com/gke-release/kubernetes/release/v1.33.9-gke.9999999/kubernetes-manifests.tar.gz",
			serverURL:    newServerURL,
			wantErr:      "needs a GKE bootstrap of v1.34.1-gke.1341000 or newer",
		},
		"unknown manifests release": {
			manifestsURL: "https://example.com/kubernetes-manifests.tar.gz",
			serverURL:    newServerURL,
			wantErr:      "does not name a GKE release",
		},
		"supported manifests with an old server binary launch": {
			manifestsURL: supportedManifestsURL,
			serverURL:    oldServerURL,
		},
		"supported manifests with an unknown server binary launch": {
			manifestsURL: supportedManifestsURL,
			serverURL:    "https://example.com/kubernetes-server-linux-amd64.tar.gz",
		},
		"supported manifests and server binary launch": {
			manifestsURL: supportedManifestsURL,
			serverURL:    newServerURL,
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider := makeProvider()
			nodeClass := withSwapTestImages(swapNodeClass(&v1alpha1.SwapConfig{Enabled: true}))
			sourceMetadata := computeMetadataValues(map[string]string{
				metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
				metadata.KubeEnvKey:       swapSourceKubeEnv(tc.manifestsURL, tc.serverURL),
				metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
			})

			_, err := provider.buildInstance(
				context.Background(),
				spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
				makeCluster("net", "subnet", "pods", false),
				"us-central1-a", "karpenter-swap-test",
				karpv1.CapacityTypeOnDemand, swapMachineType(), 0, "pods",
			)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// swapSourceKubeEnv builds the required source kube-env with the two release
// URLs under test.
func swapSourceKubeEnv(manifestsURL, serverBinaryURL string) string {
	return "CA_CERT: test-ca\n" +
		"KUBE_MANIFESTS_TAR_URL: " + manifestsURL + "\n" +
		"KUBERNETES_MASTER_NAME: 10.0.0.2\n" +
		"SERVER_BINARY_TAR_HASH: amd64-sha512\n" +
		"SERVER_BINARY_TAR_URL: " + serverBinaryURL + "\n"
}

func TestBuildInstance_RejectsBootDiskSwapForCustomImageFamily(t *testing.T) {
	provider := makeProvider()
	nodeClass := withSwapTestImages(swapNodeClass(&v1alpha1.SwapConfig{Enabled: true}))
	nodeClass.Spec.ImageFamily = lo.ToPtr(v1alpha1.ImageFamilyCustom)
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv,
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	_, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-swap-test",
		karpv1.CapacityTypeOnDemand, swapMachineType(), 0, "pods",
	)
	require.ErrorContains(t, err, `not supported for image family "Custom"`)
}

func TestBuildInstance_RejectsFailSwapOnTrueWithBootDiskSwap(t *testing.T) {
	provider := makeProvider()
	nodeClass := withSwapTestImages(swapNodeClass(&v1alpha1.SwapConfig{Enabled: true}))
	nodeClass.Spec.KubeletConfiguration = &v1alpha1.KubeletConfiguration{FailSwapOn: lo.ToPtr(true)}
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv,
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	_, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-swap-test",
		karpv1.CapacityTypeOnDemand, swapMachineType(), 0, "pods",
	)
	require.ErrorContains(t, err, "conflicts with kubeletConfiguration.failSwapOn true")
}

func decodeKubeletConfig(t *testing.T, instance string) kubeletconfig.KubeletConfiguration {
	t.Helper()
	var config kubeletconfig.KubeletConfiguration
	require.NoError(t, yaml.Unmarshal([]byte(instance), &config))
	return config
}

// TestBuildInstance_BootDiskSwapKubeletDefaults protects the effective kubelet
// behavior: a node that creates a swap file starts with swap enabled for pods,
// and the settings of the source pool it bootstraps from do not leak in. The
// source values are placed in the source kubelet-config, where GKE writes them.
func TestBuildInstance_BootDiskSwapKubeletDefaults(t *testing.T) {
	const sourceKubeletConfig = "failSwapOn: true\nmemorySwap:\n  swapBehavior: NoSwap\nnodeStatusUpdateFrequency: 10s\n"
	swap := &v1alpha1.SwapConfig{
		Enabled:         true,
		BootDiskProfile: &v1alpha1.SwapBootDiskProfile{SwapSizeGib: lo.ToPtr(int64(4))},
	}

	for name, tc := range map[string]struct {
		nodeClass        *v1alpha1.KubeletConfiguration
		wantFailSwapOn   bool
		wantSwapBehavior string
	}{
		"omitted settings replace the source pool's swap settings": {
			wantFailSwapOn:   false,
			wantSwapBehavior: "LimitedSwap",
		},
		"explicit NoSwap is preserved": {
			nodeClass: &v1alpha1.KubeletConfiguration{
				MemorySwap: &v1alpha1.MemorySwapConfiguration{SwapBehavior: "NoSwap"},
			},
			wantFailSwapOn:   false,
			wantSwapBehavior: "NoSwap",
		},
		"explicit failSwapOn false is preserved": {
			nodeClass:        &v1alpha1.KubeletConfiguration{FailSwapOn: lo.ToPtr(false)},
			wantFailSwapOn:   false,
			wantSwapBehavior: "LimitedSwap",
		},
	} {
		t.Run(name, func(t *testing.T) {
			nodeClass := swapNodeClass(swap)
			nodeClass.Spec.KubeletConfiguration = tc.nodeClass
			got := buildSwapInstanceFromSource(t, nodeClass, swapMachineType(), "", sourceKubeletConfig)

			config := decodeKubeletConfig(t, got.kubeletConfig)
			require.NotNil(t, config.FailSwapOn)
			require.Equal(t, tc.wantFailSwapOn, *config.FailSwapOn,
				"the source pool's failSwapOn must not decide the target node's value")
			require.Equal(t, tc.wantSwapBehavior, config.MemorySwap.SwapBehavior,
				"the source pool's swapBehavior must not decide the target node's value")
			require.Equal(t, "10s", config.NodeStatusUpdateFrequency.Duration.String(),
				"unrelated source kubelet settings must survive")
		})
	}
}

// TestBuildInstance_RetainsBootDiskSwapReservation protects the regression the
// replacement fixes: the scheduler reserves the swap file, and the serialized
// kubelet config must keep that reservation instead of the node class overlay's
// value.
func TestBuildInstance_RetainsBootDiskSwapReservation(t *testing.T) {
	provider := makeProvider()
	nodeClass := withSwapTestImages(swapNodeClass(&v1alpha1.SwapConfig{
		Enabled:         true,
		BootDiskProfile: &v1alpha1.SwapBootDiskProfile{SwapSizeGib: lo.ToPtr(int64(4))},
	}))
	nodeClass.Spec.KubeletConfiguration = &v1alpha1.KubeletConfiguration{
		KubeReserved: map[string]v1alpha1.KubeletQuantity{"ephemeral-storage": "20Gi"},
	}
	instanceType := makeNonGPUIT()
	instanceType.Overhead.KubeReserved = corev1.ResourceList{
		corev1.ResourceEphemeralStorage: resource.MustParse("24Gi"),
	}
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv,
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, instanceType, sourceMetadata,
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-swap-test",
		karpv1.CapacityTypeOnDemand, swapMachineType(), 0, "pods",
	)
	require.NoError(t, err)

	config := decodeKubeletConfig(t, metadataValue(instance.Metadata, metadata.KubeletConfigKey))
	require.Equal(t, "24Gi", config.KubeReserved["ephemeral-storage"],
		"the explicit 20Gi plus the 4Gi swap file must reach the kubelet")
	reserved := instanceType.Overhead.KubeReserved[corev1.ResourceEphemeralStorage]
	require.Equal(t, reserved.String(), config.KubeReserved["ephemeral-storage"],
		"the kubelet and the scheduler must agree on the reservation")
}

// TestBuildInstance_NoBootDiskSwapKeepsExplicitReservation checks that the swap
// fix leaves nodes without swap alone.
func TestBuildInstance_NoBootDiskSwapKeepsExplicitReservation(t *testing.T) {
	provider := makeProvider()
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			ImageFamily: lo.ToPtr(v1alpha1.ImageFamilyContainerOptimizedOS),
			KubeletConfiguration: &v1alpha1.KubeletConfiguration{
				KubeReserved: map[string]v1alpha1.KubeletQuantity{"ephemeral-storage": "20Gi"},
			},
		},
		Status: v1alpha1.GCENodeClassStatus{Images: bootDiskNodeClass("pd-balanced", nil, nil).Status.Images},
	}
	instanceType := makeNonGPUIT()
	instanceType.Overhead.KubeReserved = corev1.ResourceList{
		corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
	}
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv,
		metadata.KubeletConfigKey: "failSwapOn: true\nnodeStatusUpdateFrequency: 10s\n",
	})

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, instanceType, sourceMetadata,
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-swap-test",
		karpv1.CapacityTypeOnDemand, swapMachineType(), 0, "pods",
	)
	require.NoError(t, err)

	config := decodeKubeletConfig(t, metadataValue(instance.Metadata, metadata.KubeletConfigKey))
	require.Equal(t, "20Gi", config.KubeReserved["ephemeral-storage"])
	require.NotNil(t, config.FailSwapOn)
	require.True(t, *config.FailSwapOn, "a node without boot-disk swap keeps the source kubelet swap settings")
}

// TestBuildInstance_BootDiskSwapAutomaticSizeNeedsMachineFacts protects the
// failure mode: without the machine's memory the provider cannot resolve a size
// and must not launch a node whose swap it cannot account for.
func TestBuildInstance_BootDiskSwapAutomaticSizeNeedsMachineFacts(t *testing.T) {
	provider := makeProvider()
	nodeClass := withSwapTestImages(swapNodeClass(&v1alpha1.SwapConfig{Enabled: true}))
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv,
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})

	_, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-swap-test",
		karpv1.CapacityTypeOnDemand, nil, 0, "pods",
	)
	require.ErrorContains(t, err, "cannot derive a boot-disk swap size")
}

// TestBuildInstance_BootDiskSwapKeepsSourceMetadata makes sure the provider
// renders its own swap entries without mutating the source template it copies.
func TestBuildInstance_BootDiskSwapKeepsSourceMetadata(t *testing.T) {
	provider := makeProvider()
	const sourceSwap = "NODE_SWAP_SIZE: 100GiB\n"
	sourceMetadata := computeMetadataValues(map[string]string{
		metadata.KubeLabelsKey:    "max-pods-per-node=110,max-pods=110",
		metadata.KubeEnvKey:       requiredSourceKubeEnv + sourceSwap,
		metadata.KubeletConfigKey: "nodeStatusUpdateFrequency: 10s\n",
	})
	nodeClass := withSwapTestImages(swapNodeClass(&v1alpha1.SwapConfig{
		Enabled:         true,
		BootDiskProfile: &v1alpha1.SwapBootDiskProfile{SwapSizeGib: lo.ToPtr(int64(4))},
	}))

	instance, err := provider.buildInstance(
		context.Background(),
		spotOrOnDemandNodeClaim(), nodeClass, makeNonGPUIT(), sourceMetadata,
		makeCluster("net", "subnet", "pods", false),
		"us-central1-a", "karpenter-swap-test",
		karpv1.CapacityTypeOnDemand, swapMachineType(), 0, "pods",
	)
	require.NoError(t, err)

	require.Contains(t, kubeEnvFrom(t, instance), "NODE_SWAP_SIZE: 4GiB")
	for _, item := range sourceMetadata.Items {
		if item.Key == metadata.KubeEnvKey {
			require.Contains(t, *item.Value, "NODE_SWAP_SIZE: 100GiB",
				"the source template must keep the values the provider copied from")
		}
	}
}
