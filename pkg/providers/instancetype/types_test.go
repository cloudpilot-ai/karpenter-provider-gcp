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
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
)

// TestListEphemeralStorageCacheIsolation verifies that two GCENodeClass objects
// with different boot disk sizes but the same KubeletConfiguration receive
// independently computed ephemeral-storage overhead values from List().
//
// Without the disksHash in the cache key both calls share one entry, so the
// 30 GiB class would silently inherit the 200 GiB reservation (76 Gi) and the
// kubelet would refuse to start because 76 Gi > actual disk capacity (~25 Gi).
func TestListEphemeralStorageCacheIsolation(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()

	nodeClass200 := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{
				{Boot: true, SizeGiB: 200, Category: "hyperdisk-balanced"},
			},
		},
	}
	nodeClass30 := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{
				{Boot: true, SizeGiB: 30, Category: "hyperdisk-balanced"},
			},
		},
	}

	// First call: 200 GiB disk – populates the cache.
	its200, err := p.List(ctx, nodeClass200)
	assert.NoError(t, err)
	assert.NotEmpty(t, its200)
	ephemeral200 := its200[0].Overhead.KubeReserved.StorageEphemeral()

	// Second call: 30 GiB disk – must NOT reuse the 200 GiB cache entry.
	its30, err := p.List(ctx, nodeClass30)
	assert.NoError(t, err)
	assert.NotEmpty(t, its30)
	ephemeral30 := its30[0].Overhead.KubeReserved.StorageEphemeral()

	assert.Equal(t, int64(76)*1024*1024*1024, ephemeral200.Value(),
		"200 GiB disk should produce 76 Gi kubeReserved ephemeral-storage")
	assert.Equal(t, int64(15)*1024*1024*1024, ephemeral30.Value(),
		"30 GiB disk should produce 15 Gi kubeReserved ephemeral-storage, not the cached 76 Gi")
	assert.Equal(t, 2, p.staticInstanceTypesCache.ItemCount(),
		"each distinct disk config must produce a separate cache entry")
}

func TestListCacheKeyCoversLocalSsdMode(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})

	p := newTestProvider()
	raw := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		LocalSsdMode: v1alpha1.LocalSSDModeRawBlock,
	}}
	eph := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		LocalSsdMode: v1alpha1.LocalSSDModeEphemeral,
	}}

	_, err := p.List(ctx, raw)
	assert.NoError(t, err)
	_, err = p.List(ctx, eph)
	assert.NoError(t, err)

	assert.Equal(t, 2, p.staticInstanceTypesCache.ItemCount(),
		"different LocalSsdMode must produce different static cache entries")
}

func TestListRequiresInstanceTypesByName(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()
	p.instanceTypesByName = nil

	_, err := p.List(ctx, &v1alpha1.GCENodeClass{})
	require.EqualError(t, err, "no instance types found")
}

func TestListDeduplicatesMachineTypesByNameBeforeVariantExpansion(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()

	n2 := p.instanceTypesByName["n2-standard-4"]
	n2.Zone = lo.ToPtr("zones/us-central1-a")
	n2.SelfLink = lo.ToPtr("projects/test-project/zones/us-central1-a/machineTypes/n2-standard-4")
	n2Duplicate := &computepb.MachineType{
		Name:      lo.ToPtr("n2-standard-4"),
		GuestCpus: lo.ToPtr[int32](4),
		MemoryMb:  lo.ToPtr[int32](16384),
		Zone:      lo.ToPtr("zones/us-central1-b"),
		SelfLink:  lo.ToPtr("projects/test-project/zones/us-central1-b/machineTypes/n2-standard-4"),
	}
	c4 := &computepb.MachineType{
		Name:      lo.ToPtr("c4-standard-2"),
		GuestCpus: lo.ToPtr[int32](2),
		MemoryMb:  lo.ToPtr[int32](7680),
	}
	p.instanceTypesByName = indexInstanceTypesByName([]*computepb.MachineType{n2, c4, n2Duplicate})
	p.instanceTypesOfferings["c4-standard-2"] = sets.New("us-central1-a")

	instanceTypes, err := p.List(ctx, &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		LocalSsdMode: v1alpha1.LocalSSDModeRawBlock,
	}})
	require.NoError(t, err)
	require.Len(t, instanceTypes, 1+len(ssdCountVariants(n2)))
	assert.Equal(t, "c4-standard-2", instanceTypes[0].Name)
	for _, it := range instanceTypes[1:] {
		assert.Equal(t, "n2-standard-4", it.Name)
	}
}

func TestListDeduplicatesRegionalMachineTypesByNameWithoutVariants(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()

	e2 := p.instanceTypesByName["n2-standard-4"]
	e2.Name = lo.ToPtr("e2-standard-4")
	e2.Zone = lo.ToPtr("zones/us-central1-a")
	e2.SelfLink = lo.ToPtr("projects/test-project/zones/us-central1-a/machineTypes/e2-standard-4")
	e2Duplicate := &computepb.MachineType{
		Name:      lo.ToPtr("e2-standard-4"),
		GuestCpus: lo.ToPtr[int32](4),
		MemoryMb:  lo.ToPtr[int32](16384),
		Zone:      lo.ToPtr("zones/us-central1-b"),
		SelfLink:  lo.ToPtr("projects/test-project/zones/us-central1-b/machineTypes/e2-standard-4"),
	}
	c4 := &computepb.MachineType{
		Name:      lo.ToPtr("c4-standard-2"),
		GuestCpus: lo.ToPtr[int32](2),
		MemoryMb:  lo.ToPtr[int32](7680),
	}
	p.instanceTypesByName = indexInstanceTypesByName([]*computepb.MachineType{e2, c4, e2Duplicate})
	p.instanceTypesOfferings = map[string]sets.Set[string]{
		"c4-standard-2": sets.New("us-central1-a"),
		"e2-standard-4": sets.New("us-central1-a"),
	}

	instanceTypes, err := p.List(ctx, &v1alpha1.GCENodeClass{})
	require.NoError(t, err)
	require.Len(t, instanceTypes, 2)
	assert.Equal(t, "c4-standard-2", instanceTypes[0].Name)
	assert.Equal(t, "e2-standard-4", instanceTypes[1].Name)
}

func TestListUnavailableOfferingsDoNotGrowStaticCache(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()
	nodeClass := &v1alpha1.GCENodeClass{}

	first, err := p.List(ctx, nodeClass)
	assert.NoError(t, err)
	assert.NotEmpty(t, first)
	assert.True(t, spotOfferingAvailable(first[0].Offerings))

	p.unavailableOfferings.MarkUnavailable(ctx, "ICE", "n2-standard-4", "us-central1-a", karpv1.CapacityTypeSpot)

	second, err := p.List(ctx, nodeClass)
	assert.NoError(t, err)
	assert.NotEmpty(t, second)
	assert.False(t, spotOfferingAvailable(second[0].Offerings))
	assert.Equal(t, 1, p.staticInstanceTypesCache.ItemCount(),
		"unavailable offering changes must not create new static instance type cache entries")
}

func TestListRebuildsRequirementsWithInjectedOfferings(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()
	nodeClass := &v1alpha1.GCENodeClass{}

	first, err := p.List(ctx, nodeClass)
	assert.NoError(t, err)
	assert.NotEmpty(t, first)
	assert.Contains(t, first[0].Requirements.Get(karpv1.CapacityTypeLabelKey).Values(), karpv1.CapacityTypeSpot)

	p.unavailableOfferings.MarkUnavailable(ctx, "ICE", "n2-standard-4", "us-central1-a", karpv1.CapacityTypeSpot)

	second, err := p.List(ctx, nodeClass)
	assert.NoError(t, err)
	assert.NotEmpty(t, second)
	assert.NotContains(t, second[0].Requirements.Get(karpv1.CapacityTypeLabelKey).Values(), karpv1.CapacityTypeSpot)
	assert.Contains(t, second[0].Requirements.Get(karpv1.CapacityTypeLabelKey).Values(), karpv1.CapacityTypeOnDemand)
}

func TestListUnavailableOfferingExpiryDoesNotGrowStaticCache(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()
	nodeClass := &v1alpha1.GCENodeClass{}

	p.unavailableOfferings.MarkUnavailableWithTTL(ctx, "ICE", "n2-standard-4", "us-central1-a", karpv1.CapacityTypeSpot, time.Millisecond)

	unavailable, err := p.List(ctx, nodeClass)
	assert.NoError(t, err)
	assert.NotEmpty(t, unavailable)
	assert.False(t, spotOfferingAvailable(unavailable[0].Offerings))

	time.Sleep(2 * time.Millisecond)

	available, err := p.List(ctx, nodeClass)
	assert.NoError(t, err)
	assert.NotEmpty(t, available)
	assert.True(t, spotOfferingAvailable(available[0].Offerings))
	assert.Equal(t, 1, p.staticInstanceTypesCache.ItemCount(),
		"unavailable offering expiry must not create new static instance type cache entries")
}

func TestListDoesNotMutatePreviousResults(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()
	nodeClass := &v1alpha1.GCENodeClass{}

	first, err := p.List(ctx, nodeClass)
	assert.NoError(t, err)
	assert.NotEmpty(t, first)
	assert.True(t, spotOfferingAvailable(first[0].Offerings))

	p.unavailableOfferings.MarkUnavailable(ctx, "ICE", "n2-standard-4", "us-central1-a", karpv1.CapacityTypeSpot)

	second, err := p.List(ctx, nodeClass)
	assert.NoError(t, err)
	assert.NotEmpty(t, second)
	assert.False(t, spotOfferingAvailable(second[0].Offerings))
	assert.True(t, spotOfferingAvailable(first[0].Offerings),
		"later List calls must not mutate previously returned instance types")
}

func spotOfferingAvailable(offerings cloudprovider.Offerings) bool {
	for _, offering := range offerings {
		if offering.Requirements.Get(karpv1.CapacityTypeLabelKey).Any() == karpv1.CapacityTypeSpot {
			return offering.Available
		}
	}
	return false
}

func TestCalculateDiskConfiguration(t *testing.T) {
	tests := []struct {
		name             string
		nodeClass        *v1alpha1.GCENodeClass
		mt               *computepb.MachineType
		expectedBootGiB  int64
		expectedSSDGiB   int64
		expectedSSDCount int
	}{
		{
			name: "30GiB boot disk from nodeClass (issue #220)",
			nodeClass: &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					Disks: []v1alpha1.Disk{
						{Boot: true, SizeGiB: 30, Category: "hyperdisk-balanced"},
					},
				},
			},
			mt:               &computepb.MachineType{},
			expectedBootGiB:  30,
			expectedSSDGiB:   0,
			expectedSSDCount: 0,
		},
		{
			name:             "default 100GiB when no disks specified",
			nodeClass:        &v1alpha1.GCENodeClass{},
			mt:               &computepb.MachineType{},
			expectedBootGiB:  100,
			expectedSSDGiB:   0,
			expectedSSDCount: 0,
		},
		{
			name:      "BundledLocalSsds standard family: 2 partitions × 375 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: lo.ToPtr("n2-standard-8"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: lo.ToPtr[int32](2),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   750,
			expectedSSDCount: 2,
		},
		{
			name:      "BundledLocalSsds z3 family: 4 partitions × 3000 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: lo.ToPtr("z3-highmem-88-standardlssd"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: lo.ToPtr[int32](4),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   12000,
			expectedSSDCount: 4,
		},
		{
			name:      "BundledLocalSsds PartitionCount=0 treated as no SSDs",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: lo.ToPtr("n2-standard-8"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: lo.ToPtr[int32](0),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   0,
			expectedSSDCount: 0,
		},
		{
			name:      "c4d-highmem-8-lssd: 1 partition x 375 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: lo.ToPtr("c4d-highmem-8-lssd"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: lo.ToPtr[int32](1),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   375,
			expectedSSDCount: 1,
		},
		{
			name:      "c4d-highmem-16-lssd: 1 partition x 375 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: lo.ToPtr("c4d-highmem-16-lssd"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: lo.ToPtr[int32](1),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   375,
			expectedSSDCount: 1,
		},
		{
			name:      "explicit ssdCount=2 on n2d-standard-4 → 2 × 375 GiB",
			nodeClass: &v1alpha1.GCENodeClass{},
			mt: &computepb.MachineType{
				Name: lo.ToPtr("n2d-standard-4"),
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   750,
			expectedSSDCount: 2,
		},
		{
			name: "c4d-standard-8-lssd with custom 100 GiB boot disk",
			nodeClass: &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					Disks: []v1alpha1.Disk{
						{Boot: true, SizeGiB: 100, Category: "hyperdisk-balanced"},
					},
				},
			},
			mt: &computepb.MachineType{
				Name: lo.ToPtr("c4d-standard-8-lssd"),
				BundledLocalSsds: &computepb.BundledLocalSsds{
					PartitionCount: lo.ToPtr[int32](1),
				},
			},
			expectedBootGiB:  100,
			expectedSSDGiB:   375,
			expectedSSDCount: 1,
		},
		{
			name: "boot-disk entry alongside ignored legacy local-ssd entry",
			nodeClass: &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					Disks: []v1alpha1.Disk{
						{Boot: true, SizeGiB: 50, Category: "pd-balanced"},
						{Category: "local-ssd"},
					},
				},
			},
			mt: &computepb.MachineType{
				Name: lo.ToPtr("n2d-standard-4"),
			},
			expectedBootGiB:  50,
			expectedSSDGiB:   3 * 375,
			expectedSSDCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bootGiB, ssdGiB := calculateDiskConfigGiB(tt.nodeClass, tt.mt, tt.expectedSSDCount)
			assert.Equal(t, tt.expectedBootGiB, bootGiB, "boot disk GiB mismatch")
			assert.Equal(t, tt.expectedSSDGiB, ssdGiB, "total SSD GiB mismatch")
		})
	}
}

func TestNewInstanceTypeModeAware(t *testing.T) {
	const GiB = int64(1024) * 1024 * 1024

	const bootModeReservedGiB = int64(41)
	const bootModeCapGiB = int64(100)

	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	offerings := cloudprovider.Offerings{{
		Available: true,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		),
	}}

	cases := []struct {
		name            string
		mt              *computepb.MachineType
		mode            v1alpha1.LocalSSDMode
		count           int
		wantCapGiB      int64
		wantReservedGiB int64
	}{
		{
			name:            "n2d-standard-4 no SSDs default (RawBlock)",
			mt:              &computepb.MachineType{Name: lo.ToPtr("n2d-standard-4"), GuestCpus: lo.ToPtr[int32](4), MemoryMb: lo.ToPtr[int32](16384)},
			mode:            "",
			count:           0,
			wantCapGiB:      bootModeCapGiB,
			wantReservedGiB: bootModeReservedGiB,
		},
		{
			name:            "n2d-standard-4 count=2 RawBlock",
			mt:              &computepb.MachineType{Name: lo.ToPtr("n2d-standard-4"), GuestCpus: lo.ToPtr[int32](4), MemoryMb: lo.ToPtr[int32](16384)},
			mode:            v1alpha1.LocalSSDModeRawBlock,
			count:           2,
			wantCapGiB:      bootModeCapGiB,
			wantReservedGiB: bootModeReservedGiB,
		},
		{
			name:            "n2d-standard-4 count=2 Ephemeral",
			mt:              &computepb.MachineType{Name: lo.ToPtr("n2d-standard-4"), GuestCpus: lo.ToPtr[int32](4), MemoryMb: lo.ToPtr[int32](16384)},
			mode:            v1alpha1.LocalSSDModeEphemeral,
			count:           2,
			wantCapGiB:      750,
			wantReservedGiB: 75,
		},
		{
			name: "c4d-standard-8-lssd default (RawBlock) — bundled SSD ignored for capacity",
			mt: &computepb.MachineType{
				Name: lo.ToPtr("c4d-standard-8-lssd"), GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](32768),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](1)},
			},
			mode:            "",
			count:           0,
			wantCapGiB:      bootModeCapGiB,
			wantReservedGiB: bootModeReservedGiB,
		},
		{
			name: "c4d-standard-8-lssd Ephemeral",
			mt: &computepb.MachineType{
				Name: lo.ToPtr("c4d-standard-8-lssd"), GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](32768),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](1)},
			},
			mode:            v1alpha1.LocalSSDModeEphemeral,
			count:           1,
			wantCapGiB:      375,
			wantReservedGiB: 50,
		},
		{
			name: "c4d-standard-96-lssd Ephemeral (8 partitions)",
			mt: &computepb.MachineType{
				Name: lo.ToPtr("c4d-standard-96-lssd"), GuestCpus: lo.ToPtr[int32](96), MemoryMb: lo.ToPtr[int32](393216),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](8)},
			},
			mode:            v1alpha1.LocalSSDModeEphemeral,
			count:           8,
			wantCapGiB:      3000,
			wantReservedGiB: 100,
		},
		{
			name: "z3-highmem-88 legacy SKU Ephemeral (12 partitions × 3000 GiB)",
			mt: &computepb.MachineType{
				Name: lo.ToPtr("z3-highmem-88"), GuestCpus: lo.ToPtr[int32](88), MemoryMb: lo.ToPtr[int32](720896),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](12)},
			},
			mode:            v1alpha1.LocalSSDModeEphemeral,
			count:           12,
			wantCapGiB:      36000,
			wantReservedGiB: 100,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := &v1alpha1.GCENodeClass{
				Spec: v1alpha1.GCENodeClassSpec{
					LocalSsdMode: tc.mode,
				},
			}
			it := NewInstanceType(ctx, tc.mt, nc, "us-central1", offerings, tc.count)
			assert.NotNil(t, it, "InstanceType should be non-nil")
			cap := it.Capacity[corev1.ResourceEphemeralStorage]
			res := it.Overhead.KubeReserved[corev1.ResourceEphemeralStorage]
			assert.Equal(t, tc.wantCapGiB*GiB, cap.Value(), "ephemeral-storage capacity")
			assert.Equal(t, tc.wantReservedGiB*GiB, res.Value(), "ephemeral-storage kubeReserved")
		})
	}
}

func TestComputeRequirementsIncludesDiskTypeCompatibility(t *testing.T) {
	requirements := computeRequirements(&computepb.MachineType{
		Name:      lo.ToPtr("e2-standard-4"),
		GuestCpus: lo.ToPtr[int32](4),
		MemoryMb:  lo.ToPtr[int32](16384),
	}, cloudprovider.Offerings{{
		Available: true,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		),
	}}, "us-central1", 0)

	assert.Equal(t, corev1.NodeSelectorOpIn, requirements.Get("disk-type.gke.io/pd-balanced").Operator())
	assert.Equal(t, []string{"true"}, requirements.Get("disk-type.gke.io/pd-balanced").Values())
	assert.Equal(t, corev1.NodeSelectorOpDoesNotExist, requirements.Get("disk-type.gke.io/hyperdisk-throughput").Operator())

	supportedVolumeRequirement := scheduling.NewRequirements(
		scheduling.NewRequirement("disk-type.gke.io/pd-balanced", corev1.NodeSelectorOpIn, "true"),
	)
	unsupportedVolumeRequirement := scheduling.NewRequirements(
		scheduling.NewRequirement("disk-type.gke.io/hyperdisk-throughput", corev1.NodeSelectorOpIn, "true"),
	)

	assert.NoError(t, requirements.Intersects(supportedVolumeRequirement))
	assert.Error(t, requirements.Intersects(unsupportedVolumeRequirement))
}

func TestComputeRequirements(t *testing.T) {
	tests := []struct {
		name      string
		mt        *computepb.MachineType
		offerings cloudprovider.Offerings
		region    string
		ssdCount  int
		expected  scheduling.Requirements
	}{
		{
			name: "Standard Instance (n1-standard-1)",
			mt: &computepb.MachineType{
				Name:        lo.ToPtr("n1-standard-1"),
				GuestCpus:   lo.ToPtr[int32](1),
				MemoryMb:    lo.ToPtr[int32](3840),
				Zone:        lo.ToPtr("us-central1-a"),
				Description: lo.ToPtr("1 vCPU, 3.75 GB RAM"),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "n1-standard-1"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "3840"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "n1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"),
			),
		},
		{
			name: "ARM Instance (t2a-standard-1)",
			mt: &computepb.MachineType{
				Name:         lo.ToPtr("t2a-standard-1"),
				GuestCpus:    lo.ToPtr[int32](1),
				MemoryMb:     lo.ToPtr[int32](4096),
				Architecture: lo.ToPtr("ARM64"),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "t2a-standard-1"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "4096"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "t2a"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"),
			),
		},
		{
			name: "GPU Instance (a2-highgpu-1g)",
			mt: &computepb.MachineType{
				Name:      lo.ToPtr("a2-highgpu-1g"),
				GuestCpus: lo.ToPtr[int32](12),
				MemoryMb:  lo.ToPtr[int32](86016),
				Accelerators: []*computepb.Accelerators{
					{
						GuestAcceleratorCount: lo.ToPtr[int32](1),
						GuestAcceleratorType:  lo.ToPtr("nvidia-tesla-a100"),
					},
				},
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "a2-highgpu-1g"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "12"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "86016"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "a2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "highgpu"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "1g"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpIn, "nvidia-tesla-a100"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpIn, "nvidia-tesla-a100"),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"),
			),
		},
		{
			name: "E2 Instance (e2-medium)",
			mt: &computepb.MachineType{
				Name:      lo.ToPtr("e2-medium"),
				GuestCpus: lo.ToPtr[int32](2),
				MemoryMb:  lo.ToPtr[int32](4096),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "e2-medium"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "4096"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "e2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "medium"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "medium"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"),
			),
		},
		{
			name: "Offering with ZoneID",
			mt: &computepb.MachineType{
				Name:      lo.ToPtr("n1-standard-1"),
				GuestCpus: lo.ToPtr[int32](1),
				MemoryMb:  lo.ToPtr[int32](3840),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
						scheduling.NewRequirement(v1alpha1.LabelTopologyZoneID, corev1.NodeSelectorOpIn, "us-central1-a-id"),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "n1-standard-1"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "3840"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "n1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "1"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"),
			),
		},
		{
			name: "GPU Instance (c3d-highmem-8-lssd)",
			mt: &computepb.MachineType{
				Name:             lo.ToPtr("c3d-highmem-8-lssd"),
				GuestCpus:        lo.ToPtr[int32](8),
				MemoryMb:         lo.ToPtr[int32](65536),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](1)},
			},
			ssdCount: 1,
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "c3d-highmem-8-lssd"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "8"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "65536"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "c3d"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "highmem"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "3"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "8"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "1"),
			),
		},
		{
			name: "Configurable family with no SSDs (n2d-standard-8)",
			mt: &computepb.MachineType{
				Name:      lo.ToPtr("n2d-standard-8"),
				GuestCpus: lo.ToPtr[int32](8),
				MemoryMb:  lo.ToPtr[int32](32768),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "n2d-standard-8"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "8"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "32768"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "n2d"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "2"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "8"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"),
			),
		},
		{
			name: "Mixed-family non-bundled (c4a-standard-4)",
			mt: &computepb.MachineType{
				Name:         lo.ToPtr("c4a-standard-4"),
				GuestCpus:    lo.ToPtr[int32](4),
				MemoryMb:     lo.ToPtr[int32](16384),
				Architecture: lo.ToPtr("ARM64"),
			},
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "c4a-standard-4"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "4"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "16384"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "c4a"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "4"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "4"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"),
			),
		},
		{
			name: "Mixed-family bundled (c4a-standard-4-lssd)",
			mt: &computepb.MachineType{
				Name:             lo.ToPtr("c4a-standard-4-lssd"),
				GuestCpus:        lo.ToPtr[int32](4),
				MemoryMb:         lo.ToPtr[int32](16384),
				Architecture:     lo.ToPtr("ARM64"),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](1)},
			},
			ssdCount: 1,
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "c4a-standard-4-lssd"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "4"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "16384"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "c4a"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "standard"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "4"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "4"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "1"),
			),
		},
		{
			name: "Bundled SSDs (z3-highmem-22-standardlssd)",
			mt: &computepb.MachineType{
				Name:             lo.ToPtr("z3-highmem-22-standardlssd"),
				GuestCpus:        lo.ToPtr[int32](22),
				MemoryMb:         lo.ToPtr[int32](180224),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](2)},
			},
			ssdCount: 2,
			offerings: cloudprovider.Offerings{
				{
					Available: true,
					Requirements: scheduling.NewRequirements(
						scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
						scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					),
				},
			},
			region: "us-central1",
			expected: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "z3-highmem-22-standardlssd"),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "us-central1"),
				scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, "22"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, "180224"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, "z3"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpIn, "highmem"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpIn, "3"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpIn, "22"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
				scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "2"),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeRequirements(tt.mt, tt.offerings, tt.region, tt.ssdCount)

			// Validate keys present in got
			for key := range got {
				if _, ok := tt.expected[key]; !ok {
					// Ignore LabelTopologyZoneID if it wasn't expected (due to auto-generation with random values)
					if key == v1alpha1.LabelTopologyZoneID {
						continue
					}
					if strings.HasPrefix(key, "disk-type.gke.io/") {
						continue
					}
					t.Errorf("Unexpected key in result: %s", key)
				}
			}

			// Validate keys present in expected
			for key, req := range tt.expected {
				gotReq := got.Get(key)
				assert.NotNil(t, gotReq, "requirement %s should exist", key)
				if gotReq != nil {
					assert.Equal(t, req.Operator(), gotReq.Operator(), "operator for %s should match", key)
					if req.Operator() == corev1.NodeSelectorOpIn {
						assert.ElementsMatch(t, req.Values(), gotReq.Values(), "values for %s should match", key)
					}
				}
			}
		})
	}
}

func TestSSDCountVariantsAscending(t *testing.T) {
	cases := []struct {
		name string
		mt   *computepb.MachineType
		want []int
	}{
		{
			name: "configurable n2d at top-of-family vCPU bracket",
			mt:   &computepb.MachineType{Name: lo.ToPtr("n2d-standard-8"), GuestCpus: lo.ToPtr[int32](8)},
			want: []int{0, 1, 2, 4, 8, 16, 24},
		},
		{
			name: "configurable n2 at lower vCPU bracket",
			mt:   &computepb.MachineType{Name: lo.ToPtr("n2-standard-2"), GuestCpus: lo.ToPtr[int32](2)},
			want: []int{0, 1, 2, 4, 8, 16, 24},
		},
		{
			name: "configurable c2 small bracket",
			mt:   &computepb.MachineType{Name: lo.ToPtr("c2-standard-4"), GuestCpus: lo.ToPtr[int32](4)},
			want: []int{0, 1, 2, 4, 8},
		},
		{
			name: "bundled SKU emits the pinned count, no zero",
			mt: &computepb.MachineType{
				Name:             lo.ToPtr("c4d-standard-8-lssd"),
				GuestCpus:        lo.ToPtr[int32](8),
				BundledLocalSsds: &computepb.BundledLocalSsds{PartitionCount: lo.ToPtr[int32](1)},
			},
			want: []int{1},
		},
		{
			name: "no-SSD-only family emits {0}",
			mt:   &computepb.MachineType{Name: lo.ToPtr("e2-medium"), GuestCpus: lo.ToPtr[int32](2)},
			want: []int{0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ssdCountVariants(tc.mt)
			assert.Equal(t, tc.want, got, "variant slice mismatch")
			for i := 1; i < len(got); i++ {
				assert.Less(t, got[i-1], got[i],
					"emission must be strictly ascending; %v has %d before %d at index %d",
					got, got[i-1], got[i], i)
			}
		})
	}
}
