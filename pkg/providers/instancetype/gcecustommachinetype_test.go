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

package instancetype

import (
	"context"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
)

// fakeKubeClient serves a fixed GCECustomMachineTypeList to List calls without a live
// API server, matching the lightweight fake used in pkg/controllers/interruption.
type fakeKubeClient struct {
	client.Client
	customMachineTypes []v1alpha1.GCECustomMachineType
}

func (f *fakeKubeClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if v, ok := list.(*v1alpha1.GCECustomMachineTypeList); ok {
		v.Items = f.customMachineTypes
	}
	return nil
}

func readyCustomMachineType(name, machineType, onDemand, spot string, guestCpus, memoryMb int32, zones []string) v1alpha1.GCECustomMachineType {
	obj := v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.GCECustomMachineTypeSpec{
			MachineType: machineType,
			Prices:      v1alpha1.GCECustomMachineTypePrices{OnDemand: onDemand, Spot: spot},
		},
		Status: v1alpha1.GCECustomMachineTypeStatus{
			GuestCpus: guestCpus,
			MemoryMb:  memoryMb,
			Zones:     zones,
		},
	}
	obj.StatusConditions().SetTrue(status.ConditionReady)
	return obj
}

func TestListCustomMachineTypesSkipsNonReady(t *testing.T) {
	ready := readyCustomMachineType("ready", "n2-custom-8-24576", "0.40", "0.12", 8, 24576, []string{"us-central1-a"})

	notReady := v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: "not-ready"},
		Spec: v1alpha1.GCECustomMachineTypeSpec{
			MachineType: "n2-custom-4-8192",
			Prices:      v1alpha1.GCECustomMachineTypePrices{OnDemand: "0.20", Spot: "0.06"},
		},
	}
	notReady.StatusConditions().SetFalse(status.ConditionReady, "MachineTypeNotFound", "not found")

	p := &DefaultProvider{kubeClient: &fakeKubeClient{customMachineTypes: []v1alpha1.GCECustomMachineType{ready, notReady}}}

	machineTypes, prices, err := p.listCustomMachineTypes(context.Background())
	require.NoError(t, err)

	assert.Len(t, machineTypes, 1, "only the Ready registration's zones should be synthesized")
	assert.Equal(t, "n2-custom-8-24576", lo.FromPtr(machineTypes[0].Name))
	assert.Equal(t, int32(8), lo.FromPtr(machineTypes[0].GuestCpus))
	assert.Equal(t, int32(24576), lo.FromPtr(machineTypes[0].MemoryMb))
	assert.Equal(t, "us-central1-a", lo.FromPtr(machineTypes[0].Zone))

	assert.Len(t, prices, 1)
	assert.Equal(t, customMachineTypePrice{onDemand: 0.40, spot: 0.12}, prices["n2-custom-8-24576"])
	_, notReadyPriced := prices["n2-custom-4-8192"]
	assert.False(t, notReadyPriced, "a non-Ready registration must not be priced or scheduled onto")
}

func TestListCustomMachineTypesOneEntryPerZone(t *testing.T) {
	ready := readyCustomMachineType("ready", "n2-custom-8-24576", "0.40", "0.12", 8, 24576,
		[]string{"us-central1-a", "us-central1-b"})
	p := &DefaultProvider{kubeClient: &fakeKubeClient{customMachineTypes: []v1alpha1.GCECustomMachineType{ready}}}

	machineTypes, _, err := p.listCustomMachineTypes(context.Background())
	require.NoError(t, err)
	assert.Len(t, machineTypes, 2, "one synthesized entry per confirmed-available zone, matching aggregatedList's own shape")
}

func TestListCustomMachineTypesSkipsUnparsablePrice(t *testing.T) {
	bad := readyCustomMachineType("bad", "n2-custom-8-24576", "not-a-number", "0.12", 8, 24576, []string{"us-central1-a"})
	p := &DefaultProvider{kubeClient: &fakeKubeClient{customMachineTypes: []v1alpha1.GCECustomMachineType{bad}}}

	machineTypes, prices, err := p.listCustomMachineTypes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, machineTypes)
	assert.Empty(t, prices)
}

// TestListSchedulesOntoRegisteredCustomMachineType is an end-to-end regression test for issue
// #144 under the CRD-based design (proposals/0007): a registered, Ready GCECustomMachineType
// must be schedulable via List(), priced from its own registration rather than the regular
// pricing provider, and matchable by ordinary CPU/memory requirements - not only by an exact
// instance-type name - since it now joins the catalog like any predefined shape.
func TestListSchedulesOntoRegisteredCustomMachineType(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	ready := readyCustomMachineType("n2-custom-8-24576", "n2-custom-8-24576", "0.40", "0.12", 8, 24576, []string{"us-central1-a"})

	p := &DefaultProvider{
		authOptions:              &auth.Credential{Region: "us-central1"},
		pricingProvider:          &fakePricingProvider{},
		gkeProvider:              &fakeGKEProvider{},
		kubeClient:               &fakeKubeClient{customMachineTypes: []v1alpha1.GCECustomMachineType{ready}},
		instanceTypesOfferings:   map[string]sets.Set[string]{},
		unavailableOfferings:     unavailableofferings.NewUnavailableOfferings(),
		staticInstanceTypesCache: cache.New(StaticInstanceTypesCacheTTL, staticInstanceTypesCacheCleanup),
	}

	// Exercise the same merge logic UpdateInstanceTypes/UpdateInstanceTypeOfferings use,
	// without a live machineTypesClient for the (here empty) aggregated-list side.
	customTypes, customPrices, err := p.listCustomMachineTypes(ctx)
	require.NoError(t, err)
	p.instanceTypesByName = indexInstanceTypesByName(customTypes)
	p.customMachineTypePrices = customPrices
	for _, mt := range customTypes {
		zone := lo.FromPtr(mt.Zone)
		ofs, ok := p.instanceTypesOfferings[lo.FromPtr(mt.Name)]
		if !ok {
			ofs = sets.New[string]()
		}
		p.instanceTypesOfferings[lo.FromPtr(mt.Name)] = ofs.Insert(zone)
	}

	its, err := p.List(ctx, &v1alpha1.GCENodeClass{})
	require.NoError(t, err)

	custom, ok := lo.Find(its, func(it *cloudprovider.InstanceType) bool { return it.Name == "n2-custom-8-24576" })
	require.True(t, ok, "a registered, Ready custom machine type must be schedulable")
	assert.NotEmpty(t, custom.Offerings.Available())
	odOffering, ok := lo.Find(custom.Offerings, func(o *cloudprovider.Offering) bool {
		return o.Requirements.Get(karpv1.CapacityTypeLabelKey).Any() == karpv1.CapacityTypeOnDemand
	})
	require.True(t, ok)
	assert.Equal(t, 0.40, odOffering.Price, "price must come from the registration, not the (fake, always-1.0) pricing provider")

	// Matchable by CPU/memory requirements alone, not only by exact instance-type name -
	// the gap the CRD-based design closes relative to the exact-name-only mechanism.
	assert.Equal(t, "8", custom.Requirements.Get(v1alpha1.LabelInstanceCPU).Any())
	assert.Equal(t, "24576", custom.Requirements.Get(v1alpha1.LabelInstanceMemory).Any())
}
