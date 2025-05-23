/*
Copyright 2024 The CloudPilot AI Authors.

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
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/patrickmn/go-cache"
	"google.golang.org/api/iterator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing"
)

const (
	InstanceTypesCacheKey = "gce-instancetypes"
	InstanceTypesCacheTTL = 23 * time.Hour
	oneMiB                = 1024 * 1024
)

// ZoneData contains information about a GCE zone and its availability
type ZoneData struct {
	ID            string
	Available     bool
	SpotAvailable bool
}

type Provider interface {
	LivenessProbe(*http.Request) error
	List(context.Context, *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error)
	UpdateInstanceTypes(ctx context.Context) error
	UpdateInstanceTypeOfferings(ctx context.Context) error
}

type DefaultProvider struct {
	authOptions        *auth.Credential
	machineTypesClient *compute.MachineTypesClient
	pricingProvider    pricing.Provider

	muCache            sync.Mutex
	instanceTypesCache *cache.Cache

	unavailableOfferings *pkgcache.UnavailableOfferings
}

func NewDefaultProvider(ctx context.Context, authOptions *auth.Credential, pricingProvider pricing.Provider,
	unavailableOfferingsCache *pkgcache.UnavailableOfferings) *DefaultProvider {
	machineTypesClient, err := compute.NewMachineTypesRESTClient(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create default provider for node pool template")
		os.Exit(1)
	}
	return &DefaultProvider{
		authOptions:          authOptions,
		machineTypesClient:   machineTypesClient,
		pricingProvider:      pricingProvider,
		instanceTypesCache:   cache.New(InstanceTypesCacheTTL, pkgcache.DefaultCleanupInterval),
		unavailableOfferings: unavailableOfferingsCache,
	}
}

func (p *DefaultProvider) LivenessProbe(req *http.Request) error {
	return p.pricingProvider.LivenessProbe(req)
}

func (p *DefaultProvider) List(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error) {
	vmTypes, err := p.getInstanceTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing instance types: %w", err)
	}

	instanceTypes := []*cloudprovider.InstanceType{}
	for _, mt := range vmTypes {
		if mt.Name == nil || mt.MemoryMb == nil || mt.GuestCpus == nil {
			continue
		}

		memory := int64(*mt.MemoryMb) * 1024 * 1024 // MiB -> Bytes
		cpu := int64(*mt.GuestCpus)
		isSharedCore := strings.Contains(*mt.Name, "e2-micro") || strings.Contains(*mt.Name, "e2-small") || strings.Contains(*mt.Name, "e2-medium")

		kubeMem, evictionMem := calculateMemoryOverheadBreakdown(memory)
		overhead := cloudprovider.InstanceTypeOverhead{
			KubeReserved: corev1.ResourceList{
				corev1.ResourceCPU:    calculateCPUOverhead(cpu, isSharedCore),
				corev1.ResourceMemory: kubeMem,
			},
			EvictionThreshold: corev1.ResourceList{
				corev1.ResourceMemory: evictionMem,
			},
			SystemReserved: corev1.ResourceList{},
		}

		instanceTypes = append(instanceTypes, &cloudprovider.InstanceType{
			Name: *mt.Name,
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(cpu, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memory, resource.BinarySI),
			},
			Overhead:     &overhead,
			Requirements: scheduling.NewRequirements(),
		})
	}

	log.FromContext(ctx).WithValues("count", len(instanceTypes)).Info("listed GCE instance types")
	return instanceTypes, nil
}

// createOfferings creates a set of mutually exclusive offerings for a given instance type. This provider maintains an
// invariant that each offering is mutually exclusive. Specifically, there is an offering for each permutation of zone
// and capacity type. ZoneID is also injected into the offering requirements, when available, but there is a 1-1
// mapping between zone and zoneID so this does not change the number of offerings.
//
// Each requirement on the offering is guaranteed to have a single value. To get the value for a requirement on an
// offering, you can do the following thanks to this invariant:
//
//	offering.Requirements.Get(v1.TopologyLabelZone).Any()
func (p *DefaultProvider) createOfferings(_ context.Context, instanceType string, zones []ZoneData) []cloudprovider.Offering {
	var offerings []cloudprovider.Offering
	for _, zone := range zones {
		if !zone.Available {
			continue
		}

		odPrice, odOK := p.pricingProvider.OnDemandPrice(instanceType)
		spotPrice, spotOK := p.pricingProvider.SpotPrice(instanceType, zone.ID)

		if odOK {
			isUnavailable := p.unavailableOfferings.IsUnavailable(instanceType, zone.ID, karpv1.CapacityTypeOnDemand)
			offeringAvailable := !isUnavailable && zone.Available

			offerings = append(offerings, p.createOffering(zone.ID, karpv1.CapacityTypeOnDemand, odPrice, offeringAvailable))
		}

		if spotOK && zone.SpotAvailable {
			isUnavailable := p.unavailableOfferings.IsUnavailable(instanceType, zone.ID, karpv1.CapacityTypeSpot)
			offeringAvailable := !isUnavailable && zone.Available

			offerings = append(offerings, p.createOffering(zone.ID, karpv1.CapacityTypeSpot, spotPrice, offeringAvailable))
		}
	}

	return offerings
}

func (p *DefaultProvider) createOffering(zone, capacityType string, price float64, available bool) cloudprovider.Offering {
	return cloudprovider.Offering{
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, capacityType),
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
			scheduling.NewRequirement(v1alpha1.LabelTopologyZoneID, corev1.NodeSelectorOpIn, zone),
		),
		Price:     price,
		Available: available,
	}
}

func (p *DefaultProvider) UpdateInstanceTypeOfferings(ctx context.Context) error {
	types, err := p.getInstanceTypes(ctx)
	if err != nil {
		return fmt.Errorf("getting instance type offerings: %w", err)
	}
	log.FromContext(ctx).WithValues("count", len(types)).Info("updated instance type offerings from aggregated list")
	return nil
}

func (p *DefaultProvider) UpdateInstanceTypes(ctx context.Context) error {
	types, err := p.getInstanceTypes(ctx)
	if err != nil {
		return fmt.Errorf("getting instance types: %w", err)
	}
	log.FromContext(ctx).WithValues("count", len(types)).Info("discovered GCE instance types")
	return nil
}

func (p *DefaultProvider) getInstanceTypes(ctx context.Context) ([]*computepb.MachineType, error) {
	if cached, ok := p.instanceTypesCache.Get(InstanceTypesCacheKey); ok {
		return cached.([]*computepb.MachineType), nil
	}

	vmFilter := fmt.Sprintf("(zone eq .*%s-.*)", p.authOptions.Region)
	req := &computepb.AggregatedListMachineTypesRequest{
		Project: p.authOptions.ProjectID,
		Filter:  &vmFilter,
	}

	it := p.machineTypesClient.AggregatedList(ctx, req)

	var vmTypes []*computepb.MachineType
	for {
		resp, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}

		if err != nil {
			return nil, err
		}

		vmTypes = append(vmTypes, resp.Value.MachineTypes...)
	}

	p.instanceTypesCache.SetDefault(InstanceTypesCacheKey, vmTypes)
	return vmTypes, nil
}

// calculated with https://cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes#memory_reservations
func calculateMemoryOverheadBreakdown(totalMemoryBytes int64) (kubeReserved, evictionThreshold resource.Quantity) {
	mib := float64(totalMemoryBytes) / oneMiB
	var reserved float64

	if mib < 1024 {
		reserved = 255
	} else {
		memLeft := mib

		step := math.Min(memLeft, 4096)
		reserved += step * 0.25
		memLeft -= step

		step = math.Min(memLeft, 4096)
		reserved += step * 0.20
		memLeft -= step

		step = math.Min(memLeft, 8192)
		reserved += step * 0.10
		memLeft -= step

		step = math.Min(memLeft, 114688)
		reserved += step * 0.06
		memLeft -= step

		if memLeft > 0 {
			reserved += memLeft * 0.02
		}
	}

	return *resource.NewQuantity(int64(reserved*oneMiB), resource.BinarySI),
		*resource.NewQuantity(100*oneMiB, resource.BinarySI)
}

// calculated with https://cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes#cpu_reservations
func calculateCPUOverhead(vcpus int64, isSharedCore bool) resource.Quantity {
	if isSharedCore {
		return resource.MustParse("1060m")
	}

	var reservedMillicores float64

	if vcpus >= 1 {
		// 6% of first core
		reservedMillicores += 1000 * 0.06
	}

	if vcpus >= 2 {
		// 1% of next core
		reservedMillicores += 1000 * 0.01
	}

	if vcpus >= 4 {
		// 0.5% of next 2 cores
		reservedMillicores += 2000 * 0.005
	}

	if vcpus > 4 {
		// 0.25% of remaining
		reservedMillicores += float64(vcpus-4) * 1000 * 0.0025
	}

	return *resource.NewMilliQuantity(int64(reservedMillicores), resource.DecimalSI)
}
