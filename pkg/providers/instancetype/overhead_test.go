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

package instancetype

import (
	"context"
	"math"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
)

func TestKCSystemReserved(t *testing.T) {
	computed := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
	tests := []struct {
		name string
		kc   *v1alpha1.KubeletConfiguration
		want corev1.ResourceList
	}{
		{name: "nil → computed", kc: nil, want: computed},
		{
			name: "user values add to computed",
			kc: &v1alpha1.KubeletConfiguration{
				SystemReserved: map[string]v1alpha1.KubeletQuantity{"cpu": "1", "memory": "1Gi"},
			},
			want: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1100m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kcSystemReserved(computed, tt.kc)
			assert.True(t, quantityListsEqual(tt.want, got), "want %v, got %v", tt.want, got)
			assert.Equal(t, "100m", computed.Cpu().String())
		})
	}
}

func TestMergeKubeReserved(t *testing.T) {
	computed := corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("100m"),
		corev1.ResourceMemory:           resource.MustParse("200Mi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("15Gi"),
	}
	tests := []struct {
		name string
		kc   *v1alpha1.KubeletConfiguration
		want corev1.ResourceList
	}{
		{name: "nil → computed survives", kc: nil, want: computed},
		{
			// Issue #220 guard: partial user override must preserve
			// provider-computed sub-keys (ephemeral-storage, memory) so a
			// user setting kubeReserved.cpu does not crash the kubelet.
			name: "partial user override preserves computed sub-keys (#220)",
			kc: &v1alpha1.KubeletConfiguration{
				KubeReserved: map[string]v1alpha1.KubeletQuantity{"cpu": "500m"},
			},
			want: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("500m"),
				corev1.ResourceMemory:           resource.MustParse("200Mi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("15Gi"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeKubeReserved(computed, tt.kc)
			assert.True(t, quantityListsEqual(tt.want, got), "want %v, got %v", tt.want, got)
			// Defensive: must not mutate input.
			assert.Equal(t, "100m", computed.Cpu().String())
			assert.Equal(t, "15Gi", computed.StorageEphemeral().String())
		})
	}
}

func TestEvictionThreshold(t *testing.T) {
	mem10Gi := resource.MustParse("10Gi")
	storage100Gi := resource.MustParse("100Gi")
	computed := corev1.ResourceList{
		corev1.ResourceMemory:           resource.MustParse("100Mi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
	}
	tests := []struct {
		name        string
		kc          *v1alpha1.KubeletConfiguration
		wantMemory  resource.Quantity
		wantStorage resource.Quantity
	}{
		{
			// evictionSoft is intentionally not modeled in overhead — only
			// evictionHard is, matching AWS and the Kubernetes
			// node-pressure-eviction docs. Also covers nil-kc and unmodelled
			// signals (imagefs.available, pid.available) — all paths that
			// must leave computed untouched.
			name:        "evictionSoft and unmodelled signals → computed survives",
			kc:          &v1alpha1.KubeletConfiguration{EvictionSoft: map[string]v1alpha1.KubeletQuantity{"memory.available": "1Gi"}},
			wantMemory:  resource.MustParse("100Mi"),
			wantStorage: resource.MustParse("10Gi"),
		},
		{
			// 5% of 10 GiB. math.Ceil mirrors production rounding so the
			// expectation tracks the implementation for any percentage.
			name:        "evictionHard memory.available % → overhead reflects %",
			kc:          &v1alpha1.KubeletConfiguration{EvictionHard: map[string]v1alpha1.KubeletQuantity{"memory.available": "5%"}},
			wantMemory:  *resource.NewQuantity(int64(math.Ceil(0.05*float64(mem10Gi.Value()))), resource.BinarySI),
			wantStorage: resource.MustParse("10Gi"),
		},
		{
			name:        "evictionHard nodefs.available % → overhead reflects %",
			kc:          &v1alpha1.KubeletConfiguration{EvictionHard: map[string]v1alpha1.KubeletQuantity{"nodefs.available": "10%"}},
			wantMemory:  resource.MustParse("100Mi"),
			wantStorage: *resource.NewQuantity(int64(math.Ceil(0.10*float64(storage100Gi.Value()))), resource.BinarySI),
		},
		{
			// Both set → evictionHard wins; evictionSoft remains ignored.
			name: "evictionHard and evictionSoft both set → evictionHard wins",
			kc: &v1alpha1.KubeletConfiguration{
				EvictionHard: map[string]v1alpha1.KubeletQuantity{"memory.available": "500Mi"},
				EvictionSoft: map[string]v1alpha1.KubeletQuantity{"memory.available": "1Gi"},
			},
			wantMemory:  resource.MustParse("500Mi"),
			wantStorage: resource.MustParse("10Gi"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evictionThreshold(&mem10Gi, &storage100Gi, computed, tt.kc)
			gotMem := got[corev1.ResourceMemory]
			gotStor := got[corev1.ResourceEphemeralStorage]
			assert.True(t, tt.wantMemory.Cmp(gotMem) == 0, "memory: want %s, got %s", tt.wantMemory.String(), gotMem.String())
			assert.True(t, tt.wantStorage.Cmp(gotStor) == 0, "storage: want %s, got %s", tt.wantStorage.String(), gotStor.String())
			// Defensive: must not mutate input.
			computedMem := computed[corev1.ResourceMemory]
			assert.Equal(t, "100Mi", computedMem.String())
		})
	}
}

func TestComputeEvictionSignal(t *testing.T) {
	capacity1000 := *resource.NewQuantity(1000, resource.DecimalSI)
	capacity10Gi := resource.MustParse("10Gi")
	tests := []struct {
		name     string
		capacity resource.Quantity
		signal   string
		want     resource.Quantity
	}{
		{name: "percentage", capacity: capacity1000, signal: "10%", want: *resource.NewQuantity(100, resource.DecimalSI)},
		{name: "absolute quantity", capacity: capacity10Gi, signal: "500Mi", want: resource.MustParse("500Mi")},
		{name: "malformed percentage → defensive zero", capacity: capacity1000, signal: "%", want: resource.Quantity{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeEvictionSignal(tt.capacity, tt.signal)
			assert.True(t, tt.want.Cmp(got) == 0, "want %s, got %s", tt.want.String(), got.String())
		})
	}
}

func TestKCMaxPods(t *testing.T) {
	tests := []struct {
		name             string
		nodeClassMaxPods int32
		podsPerCore      *int32
		cpus             int64
		want             int64
	}{
		{name: "podsPerCore unset → nodeClassMaxPods unchanged", nodeClassMaxPods: 110, podsPerCore: nil, cpus: 4, want: 110},
		{name: "podsPerCore=0 → nodeClassMaxPods unchanged", nodeClassMaxPods: 110, podsPerCore: lo.ToPtr[int32](0), cpus: 4, want: 110},
		{name: "podsPerCore × cpus < max → podsPerCore wins", nodeClassMaxPods: 110, podsPerCore: lo.ToPtr[int32](8), cpus: 2, want: 16},
		{name: "podsPerCore × cpus ≥ max → max wins", nodeClassMaxPods: 64, podsPerCore: lo.ToPtr[int32](8), cpus: 16, want: 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var kc *v1alpha1.KubeletConfiguration
			if tt.podsPerCore != nil {
				kc = &v1alpha1.KubeletConfiguration{PodsPerCore: tt.podsPerCore}
			}
			got := kcMaxPods(tt.nodeClassMaxPods, kc, tt.cpus)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestNewInstanceType_RespectsKubeletConfiguration is the integration-level
// guard that the per-field helpers are wired correctly into NewInstanceType.
// It covers the #398 fix (SystemReserved verbatim, KubeReserved merged with
// the #220 ephemeral-storage survival, PodsPerCore caps Capacity[pods]) and
// the nil-kc baseline in a single table.
func TestNewInstanceType_IncludesStaticKubeProxyOverhead(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	mt := &computepb.MachineType{
		Name:      lo.ToPtr("n2-standard-1"),
		GuestCpus: lo.ToPtr[int32](1),
		MemoryMb:  lo.ToPtr[int32](4096),
	}

	it := NewInstanceType(ctx, mt, &v1alpha1.GCENodeClass{}, "us-central1", testOfferings())

	assert.Equal(t, int64(60), it.Overhead.KubeReserved.Cpu().MilliValue())
	assert.Equal(t, int64(staticKubeProxyCPUMilliCore), it.Overhead.SystemReserved.Cpu().MilliValue())
}

func TestNewInstanceType_RespectsKubeletConfiguration(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	mt := &computepb.MachineType{
		Name:      lo.ToPtr("n2-standard-4"),
		GuestCpus: lo.ToPtr[int32](4),
		MemoryMb:  lo.ToPtr[int32](16384),
	}
	tests := []struct {
		name   string
		kc     *v1alpha1.KubeletConfiguration
		assert func(t *testing.T, it *cloudprovider.InstanceType)
	}{
		{
			name: "nil kc → static kube-proxy SystemReserved, KubeReserved computed, default max pods",
			kc:   nil,
			assert: func(t *testing.T, it *cloudprovider.InstanceType) {
				assert.Equal(t, int64(staticKubeProxyCPUMilliCore), it.Overhead.SystemReserved.Cpu().MilliValue())
				assert.Equal(t, int64(80), it.Overhead.KubeReserved.Cpu().MilliValue())
				assert.Greater(t, it.Overhead.KubeReserved.Memory().Value(), int64(0))
				pods := it.Capacity[corev1.ResourcePods]
				assert.Equal(t, int64(v1alpha1.KubeletMaxPods), pods.Value())
			},
		},
		{
			// #398 + #220 integration guard: SystemReserved verbatim;
			// KubeReserved.cpu user-overridden; KubeReserved.memory and
			// .ephemeral-storage survive from provider-computed defaults.
			name: "user reservations honored with computed sub-keys surviving (#398, #220)",
			kc: &v1alpha1.KubeletConfiguration{
				SystemReserved: map[string]v1alpha1.KubeletQuantity{"cpu": "1", "memory": "1Gi"},
				KubeReserved:   map[string]v1alpha1.KubeletQuantity{"cpu": "500m"},
			},
			assert: func(t *testing.T, it *cloudprovider.InstanceType) {
				assert.Equal(t, "1100m", it.Overhead.SystemReserved.Cpu().String())
				assert.Equal(t, "1Gi", it.Overhead.SystemReserved.Memory().String())
				assert.Equal(t, "500m", it.Overhead.KubeReserved.Cpu().String())
				assert.Greater(t, it.Overhead.KubeReserved.Memory().Value(), int64(0))
				assert.Greater(t, it.Overhead.KubeReserved.StorageEphemeral().Value(), int64(0))
			},
		},
		{
			name: "PodsPerCore caps Capacity[pods]",
			kc:   &v1alpha1.KubeletConfiguration{PodsPerCore: lo.ToPtr[int32](8)},
			assert: func(t *testing.T, it *cloudprovider.InstanceType) {
				pods := it.Capacity[corev1.ResourcePods]
				assert.Equal(t, int64(32), pods.Value(), "8 podsPerCore × 4 cpus")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{KubeletConfiguration: tt.kc}}
			it := NewInstanceType(ctx, mt, nodeClass, "us-central1", testOfferings())
			assert.NotNil(t, it)
			tt.assert(t, it)
		})
	}
}

// quantityListsEqual compares two ResourceLists by Quantity.Cmp so 1Gi vs
// 1024Mi compare equal regardless of Format.
func quantityListsEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av.Cmp(bv) != 0 {
			return false
		}
	}
	return true
}

func testOfferings() cloudprovider.Offerings {
	return cloudprovider.Offerings{
		{
			Available: true,
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			),
		},
	}
}
