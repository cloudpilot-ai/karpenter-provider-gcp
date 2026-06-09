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
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

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
	require.NoError(t, RenderKubeletConfigMetadata(meta, nodeClass, it, capacityType))
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
