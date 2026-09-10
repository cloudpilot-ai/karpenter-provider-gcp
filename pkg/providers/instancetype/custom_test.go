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
	"fmt"
	"net/http"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"google.golang.org/api/googleapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
)

// notFoundErr simulates the 404 googleapi.Error machineTypes.get returns for an invalid or
// unavailable custom shape. errTransient simulates any other failure (5xx, auth, quota,
// canceled context, network error) that says nothing about whether the shape is valid.
var notFoundErr = &googleapi.Error{Code: http.StatusNotFound}
var errTransient = fmt.Errorf("rpc error: internal")

func TestIsCustomMachineTypeName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"n2-custom-8-24576", true},
		{"n2-custom-8-245760-ext", true},
		{"n1-custom-2-4096", true},
		{"e2-custom-4-8192", true},
		{"n2-standard-4", false},
		{"n2-custom", false},
		{"n2-custom-8", false},
		{"", false},
	} {
		assert.Equal(t, tc.want, isCustomMachineTypeName(tc.name), tc.name)
	}
}

func TestResolveCustomMachineTypesFetchesAndCaches(t *testing.T) {
	ctx := context.Background()
	zones := []string{"us-central1-a", "us-central1-b"}
	cacheStore := cache.New(customMachineTypeCacheTTL, customMachineTypeCacheTTL)

	calls := 0
	getMachineType := func(_ context.Context, zone, name string) (*computepb.MachineType, error) {
		calls++
		assert.Equal(t, "n2-custom-8-24576", name)
		return &computepb.MachineType{
			Name:      lo.ToPtr(name),
			GuestCpus: lo.ToPtr[int32](8),
			MemoryMb:  lo.ToPtr[int32](24576),
			Zone:      lo.ToPtr(zone),
		}, nil
	}

	machineTypes, offerings := resolveCustomMachineTypes(
		ctx, []string{"n2-custom-8-24576"}, zones, nil, cacheStore, getMachineType)

	assert.Len(t, machineTypes, 1)
	assert.Equal(t, "n2-custom-8-24576", lo.FromPtr(machineTypes[0].Name))
	assert.Equal(t, int32(8), lo.FromPtr(machineTypes[0].GuestCpus))
	assert.ElementsMatch(t, []string{"us-central1-a", "us-central1-b"}, offerings["n2-custom-8-24576"].UnsortedList())
	assert.Equal(t, 2, calls, "should query every zone once")

	// A second resolution for the same name must be served from cache, not the API.
	machineTypes, offerings = resolveCustomMachineTypes(
		ctx, []string{"n2-custom-8-24576"}, zones, nil, cacheStore, getMachineType)
	assert.Len(t, machineTypes, 1)
	assert.Len(t, offerings["n2-custom-8-24576"], 2)
	assert.Equal(t, 2, calls, "cached result must not re-query the API")
}

func TestResolveCustomMachineTypesSkipsKnownAndNonCustomNames(t *testing.T) {
	ctx := context.Background()
	zones := []string{"us-central1-a"}
	cacheStore := cache.New(customMachineTypeCacheTTL, customMachineTypeCacheTTL)
	known := []*computepb.MachineType{{Name: lo.ToPtr("n2-custom-8-24576")}}

	called := false
	getMachineType := func(_ context.Context, _, _ string) (*computepb.MachineType, error) {
		called = true
		return nil, nil
	}

	machineTypes, offerings := resolveCustomMachineTypes(
		ctx, []string{"n2-custom-8-24576", "n2-standard-4", ""}, zones, known, cacheStore, getMachineType)

	assert.Empty(t, machineTypes)
	assert.Empty(t, offerings)
	assert.False(t, called, "already-cataloged and non-custom names must never hit the API")
}

func TestResolveCustomMachineTypesInvalidShapeYieldsNoResult(t *testing.T) {
	ctx := context.Background()
	zones := []string{"us-central1-a", "us-central1-b"}
	cacheStore := cache.New(customMachineTypeCacheTTL, customMachineTypeCacheTTL)

	getMachineType := func(_ context.Context, _, _ string) (*computepb.MachineType, error) {
		return nil, notFoundErr
	}

	machineTypes, offerings := resolveCustomMachineTypes(
		ctx, []string{"n2-custom-3-24576"}, zones, nil, cacheStore, getMachineType)

	assert.Empty(t, machineTypes)
	assert.Empty(t, offerings)

	cached, ok := cacheStore.Get("n2-custom-3-24576")
	assert.True(t, ok, "a definitive not-found in every zone must still be cached to avoid repeated lookups")
	assert.Nil(t, cached.(customMachineTypeCacheEntry).machineType)
}

func TestResolveCustomMachineTypesPartialZoneAvailability(t *testing.T) {
	ctx := context.Background()
	zones := []string{"us-central1-a", "us-central1-b"}
	cacheStore := cache.New(customMachineTypeCacheTTL, customMachineTypeCacheTTL)

	getMachineType := func(_ context.Context, zone, name string) (*computepb.MachineType, error) {
		if zone == "us-central1-b" {
			// A definitive not-found: the family simply isn't offered in this zone.
			return nil, notFoundErr
		}
		return &computepb.MachineType{Name: lo.ToPtr(name), GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](24576)}, nil
	}

	machineTypes, offerings := resolveCustomMachineTypes(
		ctx, []string{"n2-custom-8-24576"}, zones, nil, cacheStore, getMachineType)

	assert.Len(t, machineTypes, 1)
	assert.ElementsMatch(t, []string{"us-central1-a"}, offerings["n2-custom-8-24576"].UnsortedList())

	cached, ok := cacheStore.Get("n2-custom-8-24576")
	assert.True(t, ok, "a clean pass (found + definitive not-found only) must be cached")
	assert.ElementsMatch(t, []string{"us-central1-a"}, cached.(customMachineTypeCacheEntry).zones.UnsortedList())
}

// TestResolveCustomMachineTypesTransientErrorNotCached is a regression test for a Greptile
// review finding on PR #601: a transient error (unlike a definitive 404) says nothing about
// whether the shape is actually valid or available, so it must not be cached as unavailable -
// doing so would misreport a temporary API failure as the shape being absent and would keep a
// valid custom type unschedulable for the negative-cache TTL even after the API recovers.
func TestResolveCustomMachineTypesTransientErrorNotCached(t *testing.T) {
	ctx := context.Background()
	zones := []string{"us-central1-a"}
	cacheStore := cache.New(customMachineTypeCacheTTL, customMachineTypeCacheTTL)

	calls := 0
	getMachineType := func(_ context.Context, _, _ string) (*computepb.MachineType, error) {
		calls++
		return nil, errTransient
	}

	machineTypes, offerings := resolveCustomMachineTypes(
		ctx, []string{"n2-custom-8-24576"}, zones, nil, cacheStore, getMachineType)
	assert.Empty(t, machineTypes)
	assert.Empty(t, offerings)

	_, ok := cacheStore.Get("n2-custom-8-24576")
	assert.False(t, ok, "a transient error must not be cached, so the next List call retries")

	// A second resolution call should hit the API again, not serve a cached negative result.
	resolveCustomMachineTypes(ctx, []string{"n2-custom-8-24576"}, zones, nil, cacheStore, getMachineType)
	assert.Equal(t, 2, calls, "each uncached resolution should re-query the API")
}

// TestResolveCustomMachineTypesPartialTransientErrorNotCached covers the "partial failure"
// half of the same Greptile finding: one zone resolves cleanly but another hits a transient
// error, so the zone set is incomplete and must not be cached, even though a machine type was
// found.
func TestResolveCustomMachineTypesPartialTransientErrorNotCached(t *testing.T) {
	ctx := context.Background()
	zones := []string{"us-central1-a", "us-central1-b"}
	cacheStore := cache.New(customMachineTypeCacheTTL, customMachineTypeCacheTTL)

	getMachineType := func(_ context.Context, zone, name string) (*computepb.MachineType, error) {
		if zone == "us-central1-b" {
			return nil, errTransient
		}
		return &computepb.MachineType{Name: lo.ToPtr(name), GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](24576)}, nil
	}

	machineTypes, offerings := resolveCustomMachineTypes(
		ctx, []string{"n2-custom-8-24576"}, zones, nil, cacheStore, getMachineType)

	// The immediate result still reflects what was confirmed so far...
	assert.Len(t, machineTypes, 1)
	assert.ElementsMatch(t, []string{"us-central1-a"}, offerings["n2-custom-8-24576"].UnsortedList())

	// ...but must not be cached, since us-central1-b's availability is still unresolved.
	_, ok := cacheStore.Get("n2-custom-8-24576")
	assert.False(t, ok, "an incomplete zone set must not be cached")
}

// TestListDiscoversRequestedCustomMachineType is a regression test for issue #144: a
// NodePool pinned to an exact custom machine type (e.g. n2-custom-8-24576) that already
// runs successfully in the cluster was reported as having zero compatible instance types,
// because custom shapes never appear in the machineTypes.aggregatedList catalog that
// populates instanceTypesInfo. List must resolve such names on demand instead.
func TestListDiscoversRequestedCustomMachineType(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	p := newTestProvider()
	p.getMachineType = func(_ context.Context, zone, name string) (*computepb.MachineType, error) {
		assert.Equal(t, "n2-custom-8-24576", name)
		return &computepb.MachineType{
			Name:      lo.ToPtr(name),
			GuestCpus: lo.ToPtr[int32](8),
			MemoryMb:  lo.ToPtr[int32](24576),
			Zone:      lo.ToPtr(zone),
		}, nil
	}
	nodeClass := &v1alpha1.GCENodeClass{}

	its, err := p.List(ctx, nodeClass, []string{"n2-custom-8-24576"})
	assert.NoError(t, err)

	custom, ok := lo.Find(its, func(it *cloudprovider.InstanceType) bool { return it.Name == "n2-custom-8-24576" })
	assert.True(t, ok, "requested custom machine type must be present in the returned instance types")
	assert.Equal(t, "8", custom.Requirements.Get(v1alpha1.LabelInstanceCPU).Any())
	assert.True(t, custom.Offerings.Available()[0].Requirements.Get(corev1.LabelTopologyZone).Has("us-central1-a"))

	// A NodePool that never asked for it must not see it.
	itsWithoutRequest, err := p.List(ctx, nodeClass, nil)
	assert.NoError(t, err)
	_, found := lo.Find(itsWithoutRequest, func(it *cloudprovider.InstanceType) bool { return it.Name == "n2-custom-8-24576" })
	assert.False(t, found, "an unrequested custom machine type must not be synthesized")
}

func TestDeriveCustomPrice(t *testing.T) {
	// Real n2 on-demand prices (africa-south1), which are exactly linear in vCPU count per
	// shape: standard is billed at 4 GiB/vCPU, highmem at 8 GiB/vCPU. c is $/vCPU-hour, m is
	// $/MB-hour; solving c + 4096m = 0.053417 and c + 8192m = 0.072061 gives the expected
	// price below for an 8 vCPU / 24576 MB custom shape.
	p := &DefaultProvider{
		pricingProvider: &staticPricingProvider{
			onDemand: map[string]float64{
				"n2-standard-2": 0.106834,
				"n2-standard-4": 0.213668,
				"n2-highmem-2":  0.144122,
				"n2-highmem-4":  0.288244,
			},
		},
		instanceTypesInfo: []*computepb.MachineType{
			{Name: lo.ToPtr("n2-standard-2"), GuestCpus: lo.ToPtr[int32](2), MemoryMb: lo.ToPtr[int32](8192)},
			{Name: lo.ToPtr("n2-standard-4"), GuestCpus: lo.ToPtr[int32](4), MemoryMb: lo.ToPtr[int32](16384)},
			{Name: lo.ToPtr("n2-highmem-2"), GuestCpus: lo.ToPtr[int32](2), MemoryMb: lo.ToPtr[int32](16384)},
			{Name: lo.ToPtr("n2-highmem-4"), GuestCpus: lo.ToPtr[int32](4), MemoryMb: lo.ToPtr[int32](32768)},
		},
	}

	price, ok := p.deriveCustomPrice(&computepb.MachineType{
		Name:      lo.ToPtr("n2-custom-8-24576"),
		GuestCpus: lo.ToPtr[int32](8),
		MemoryMb:  lo.ToPtr[int32](24576),
	})
	assert.True(t, ok)
	assert.InDelta(t, 0.390074, price, 0.0001)
}

func TestDeriveCustomPriceInsufficientData(t *testing.T) {
	p := &DefaultProvider{
		pricingProvider: &staticPricingProvider{
			onDemand: map[string]float64{"n2-standard-2": 0.106834},
		},
		instanceTypesInfo: []*computepb.MachineType{
			{Name: lo.ToPtr("n2-standard-2"), GuestCpus: lo.ToPtr[int32](2), MemoryMb: lo.ToPtr[int32](8192)},
		},
	}

	_, ok := p.deriveCustomPrice(&computepb.MachineType{
		Name:      lo.ToPtr("n2-custom-8-24576"),
		GuestCpus: lo.ToPtr[int32](8),
		MemoryMb:  lo.ToPtr[int32](24576),
	})
	assert.False(t, ok, "a single known sibling price cannot calibrate the two-parameter model")
}

// TestListDerivesPriceForCustomMachineTypeWithoutPublishedPrice is a regression test: GCP's
// pricing data never has an entry for a custom shape's exact name (only predefined catalog
// names), so a pricing provider that behaves like the real one - returning ok=false for an
// unknown name, rather than fakePricingProvider's fixed price for every name - must not
// cause the resolved custom machine type to be dropped for lack of offerings.
func TestListDerivesPriceForCustomMachineTypeWithoutPublishedPrice(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	standard2 := &computepb.MachineType{Name: lo.ToPtr("n2-standard-2"), GuestCpus: lo.ToPtr[int32](2), MemoryMb: lo.ToPtr[int32](8192)}
	highmem2 := &computepb.MachineType{Name: lo.ToPtr("n2-highmem-2"), GuestCpus: lo.ToPtr[int32](2), MemoryMb: lo.ToPtr[int32](16384)}

	p := &DefaultProvider{
		authOptions: &auth.Credential{Region: "us-central1"},
		pricingProvider: &staticPricingProvider{
			onDemand: map[string]float64{"n2-standard-2": 0.106834, "n2-highmem-2": 0.144122},
		},
		gkeProvider:              &fakeGKEProvider{},
		instanceTypesInfo:        []*computepb.MachineType{standard2, highmem2},
		instanceTypesOfferings:   map[string]sets.Set[string]{},
		unavailableOfferings:     unavailableofferings.NewUnavailableOfferings(),
		staticInstanceTypesCache: cache.New(StaticInstanceTypesCacheTTL, staticInstanceTypesCacheCleanup),
		customMachineTypesCache:  cache.New(customMachineTypeCacheTTL, staticInstanceTypesCacheCleanup),
		cm:                       pretty.NewChangeMonitor(),
		getMachineType: func(_ context.Context, zone, name string) (*computepb.MachineType, error) {
			return &computepb.MachineType{
				Name:      lo.ToPtr(name),
				GuestCpus: lo.ToPtr[int32](8),
				MemoryMb:  lo.ToPtr[int32](24576),
				Zone:      lo.ToPtr(zone),
			}, nil
		},
	}

	its, err := p.List(ctx, &v1alpha1.GCENodeClass{}, []string{"n2-custom-8-24576"})
	assert.NoError(t, err)

	custom, ok := lo.Find(its, func(it *cloudprovider.InstanceType) bool { return it.Name == "n2-custom-8-24576" })
	assert.True(t, ok, "custom machine type must still be discoverable when its exact name has no published price")
	assert.NotEmpty(t, custom.Offerings.Available(), "a derived price must produce available offerings")
	odOffering, ok := lo.Find(custom.Offerings, func(o *cloudprovider.Offering) bool {
		return o.Requirements.Get(karpv1.CapacityTypeLabelKey).Any() == karpv1.CapacityTypeOnDemand
	})
	assert.True(t, ok)
	assert.InDelta(t, 0.390074, odOffering.Price, 0.0001)
}

// TestListDiscoversArbitraryCustomShapesAcrossFamilies is a generality check: nothing in the
// resolution or pricing path may be hardcoded to n2 or to the 8-vCPU/24576-MB shape used
// elsewhere in this file. It exercises several different machine families (including a
// made-up one, to prove no family allowlist exists), each with its own arbitrary vCPU/memory
// combination and its own independently-calibrated price, all through one shared provider.
func TestListDiscoversArbitraryCustomShapesAcrossFamilies(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})

	type shape struct {
		suffix        string
		gibPerCPU     float64
		pricePerCPUHr float64 // determines this shape's on-demand price at any size
	}
	families := []struct {
		family string
		shapes []shape
	}{
		// e2: well-known real GCP ratios (standard 4 GiB/vCPU, highmem 8, highcpu 1) with
		// real on-demand prices (africa-south1).
		{family: "e2", shapes: []shape{
			{suffix: "standard", gibPerCPU: 4, pricePerCPUHr: 0.07371546 / 2},
			{suffix: "highmem", gibPerCPU: 8, pricePerCPUHr: 0.09944418 / 2},
		}},
		// n4: a different family with different real ratios (standard 4 GiB/vCPU, highmem 8)
		// and different real prices, to confirm calibration isn't shared across families.
		{family: "n4", shapes: []shape{
			{suffix: "standard", gibPerCPU: 4, pricePerCPUHr: 0.09986974 / 2},
			{suffix: "highmem", gibPerCPU: 8, pricePerCPUHr: 0.13105286 / 2},
		}},
		// A made-up family name: proves the resolution path has no hardcoded family
		// allowlist and works from the regex + calibration data alone.
		{family: "zz9", shapes: []shape{
			{suffix: "standard", gibPerCPU: 3, pricePerCPUHr: 0.05},
			{suffix: "highmem", gibPerCPU: 6, pricePerCPUHr: 0.08},
		}},
	}

	// Requested custom shapes: different vCPU counts and memory sizes per family, none of
	// them the 8-vCPU/24576-MB example used elsewhere in this file.
	requested := map[string]struct {
		cpus  int32
		memMB int32
	}{
		"e2":  {cpus: 6, memMB: 18432},  // 3 GiB/vCPU: between highcpu-less standard(4) and... just an arbitrary in-between shape
		"n4":  {cpus: 12, memMB: 73728}, // 6 GiB/vCPU
		"zz9": {cpus: 5, memMB: 15360},  // 3 GiB/vCPU
	}

	onDemand := map[string]float64{}
	var instanceTypesInfo []*computepb.MachineType
	for _, f := range families {
		for _, sizeCPUs := range []int32{2, 4} {
			for _, sh := range f.shapes {
				name := fmt.Sprintf("%s-%s-%d", f.family, sh.suffix, sizeCPUs)
				memMB := int32(float64(sizeCPUs) * sh.gibPerCPU * 1024)
				onDemand[name] = sh.pricePerCPUHr * float64(sizeCPUs)
				instanceTypesInfo = append(instanceTypesInfo, &computepb.MachineType{
					Name: lo.ToPtr(name), GuestCpus: lo.ToPtr(sizeCPUs), MemoryMb: lo.ToPtr(memMB),
				})
			}
		}
	}

	var requestedNames []string
	for family := range requested {
		requestedNames = append(requestedNames, fmt.Sprintf("%s-custom-%d-%d", family, requested[family].cpus, requested[family].memMB))
	}

	p := &DefaultProvider{
		authOptions:              &auth.Credential{Region: "us-central1"},
		pricingProvider:          &staticPricingProvider{onDemand: onDemand},
		gkeProvider:              &fakeGKEProvider{},
		instanceTypesInfo:        instanceTypesInfo,
		instanceTypesOfferings:   map[string]sets.Set[string]{},
		unavailableOfferings:     unavailableofferings.NewUnavailableOfferings(),
		staticInstanceTypesCache: cache.New(StaticInstanceTypesCacheTTL, staticInstanceTypesCacheCleanup),
		customMachineTypesCache:  cache.New(customMachineTypeCacheTTL, staticInstanceTypesCacheCleanup),
		cm:                       pretty.NewChangeMonitor(),
		getMachineType: func(_ context.Context, zone, name string) (*computepb.MachineType, error) {
			for family, r := range requested {
				if name == fmt.Sprintf("%s-custom-%d-%d", family, r.cpus, r.memMB) {
					return &computepb.MachineType{Name: lo.ToPtr(name), GuestCpus: lo.ToPtr(r.cpus), MemoryMb: lo.ToPtr(r.memMB), Zone: lo.ToPtr(zone)}, nil
				}
			}
			return nil, fmt.Errorf("unexpected machine type requested: %s", name)
		},
	}

	its, err := p.List(ctx, &v1alpha1.GCENodeClass{}, requestedNames)
	assert.NoError(t, err)

	for family, r := range requested {
		name := fmt.Sprintf("%s-custom-%d-%d", family, r.cpus, r.memMB)
		custom, ok := lo.Find(its, func(it *cloudprovider.InstanceType) bool { return it.Name == name })
		if !assert.True(t, ok, "%s must be discovered", name) {
			continue
		}
		assert.NotEmpty(t, custom.Offerings.Available(), "%s must have a derived price and be schedulable", name)
		assert.Equal(t, fmt.Sprintf("%d", r.cpus), custom.Requirements.Get(v1alpha1.LabelInstanceCPU).Any())
	}
}
