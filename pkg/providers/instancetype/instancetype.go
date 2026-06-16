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
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"google.golang.org/api/iterator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing"
)

const (
	// StaticInstanceTypesCacheTTL bounds cached NodeClass-derived instance type shapes.
	// Dynamic offerings are injected on each List call and are not stored here.
	StaticInstanceTypesCacheTTL     = 30 * time.Hour
	staticInstanceTypesCacheCleanup = time.Minute
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
	gkeProvider        gke.Provider

	// We assume that all instance types with spot are available in all zones.
	// Reference: https://cloud.google.com/compute/docs/instances/provisioning-models

	muInstanceTypesInfo    sync.RWMutex
	instanceTypesInfo      []*computepb.MachineType
	instanceTypesOfferings map[string]sets.Set[string]
	instanceTypesSeqNum    uint64
	cm                     *pretty.ChangeMonitor

	unavailableOfferings *unavailableofferings.UnavailableOfferings

	staticInstanceTypesCache   *cache.Cache
	muStaticInstanceTypesCache sync.Mutex
}

type staticInstanceType struct {
	machineType  *computepb.MachineType
	instanceType *cloudprovider.InstanceType
}

func NewDefaultProvider(ctx context.Context, authOptions *auth.Credential, pricingProvider pricing.Provider,
	gkeProvider gke.Provider, unavailableOfferingsCache *unavailableofferings.UnavailableOfferings) *DefaultProvider {
	machineTypesClient, err := compute.NewMachineTypesRESTClient(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create default provider for node pool template")
		os.Exit(1)
	}
	return &DefaultProvider{
		authOptions:              authOptions,
		machineTypesClient:       machineTypesClient,
		pricingProvider:          pricingProvider,
		gkeProvider:              gkeProvider,
		staticInstanceTypesCache: cache.New(StaticInstanceTypesCacheTTL, staticInstanceTypesCacheCleanup),
		instanceTypesOfferings:   make(map[string]sets.Set[string]),
		unavailableOfferings:     unavailableOfferingsCache,
		cm:                       pretty.NewChangeMonitor(),
		instanceTypesSeqNum:      0,
	}
}

func (p *DefaultProvider) LivenessProbe(req *http.Request) error {
	return p.pricingProvider.LivenessProbe(req)
}

func (p *DefaultProvider) validateState() error {
	if len(p.instanceTypesInfo) == 0 {
		return fmt.Errorf("no instance types found")
	}

	return nil
}

func (p *DefaultProvider) List(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error) {
	p.muInstanceTypesInfo.RLock()
	defer p.muInstanceTypesInfo.RUnlock()

	if err := p.validateState(); err != nil {
		return nil, err
	}

	zones, err := p.gkeProvider.ResolveClusterZones(ctx)
	if err != nil {
		return nil, err
	}

	staticInstanceTypes, err := p.getStaticInstanceTypes(ctx, nodeClass)
	if err != nil {
		return nil, err
	}

	return p.injectOfferings(ctx, staticInstanceTypes, zones), nil
}

func (p *DefaultProvider) getStaticInstanceTypes(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) ([]staticInstanceType, error) {
	kcHash, _ := hashstructure.Hash(nodeClass.Spec.KubeletConfiguration, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	disksHash, _ := hashstructure.Hash(nodeClass.Spec.Disks, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	key := fmt.Sprintf("%d-%d-%d", atomic.LoadUint64(&p.instanceTypesSeqNum), kcHash, disksHash)

	if item, ok := p.staticInstanceTypesCache.Get(key); ok {
		return item.([]staticInstanceType), nil
	}

	p.muStaticInstanceTypesCache.Lock()
	defer p.muStaticInstanceTypesCache.Unlock()
	if item, ok := p.staticInstanceTypesCache.Get(key); ok {
		return item.([]staticInstanceType), nil
	}

	instanceTypes := []staticInstanceType{}
	for _, mt := range p.instanceTypesInfo {
		instanceType := lo.FromPtr(mt.Name)
		if instanceType == "" || mt.MemoryMb == nil || mt.GuestCpus == nil {
			continue
		}

		it := NewStaticInstanceType(ctx, mt, nodeClass)
		if it == nil {
			continue
		}
		instanceTypes = append(instanceTypes, staticInstanceType{machineType: mt, instanceType: it})
	}
	p.staticInstanceTypesCache.SetDefault(key, instanceTypes)
	return instanceTypes, nil
}

func (p *DefaultProvider) injectOfferings(ctx context.Context, staticInstanceTypes []staticInstanceType, zones []string) []*cloudprovider.InstanceType {
	instanceTypes := []*cloudprovider.InstanceType{}
	for _, cached := range staticInstanceTypes {
		instanceType := cached.instanceType.Name
		zoneData := p.buildZoneData(instanceType, zones)
		offerings := p.createOfferings(ctx, instanceType, zoneData)
		if len(offerings) == 0 {
			continue
		}

		it := cached.instanceType.DeepCopy()
		it.Offerings = offerings
		it.Requirements = computeRequirements(cached.machineType, offerings, p.authOptions.Region)
		instanceTypes = append(instanceTypes, it)
	}
	return instanceTypes
}

// buildZoneData checks zonal availability from cached offerings while keeping spot
// availability enabled for all zones per GCP spot provisioning behavior.
func (p *DefaultProvider) buildZoneData(instanceType string, zones []string) []ZoneData {
	ofs, ok := p.instanceTypesOfferings[instanceType]
	return lo.Map(zones, func(zoneID string, _ int) ZoneData {
		ret := ZoneData{ID: zoneID, Available: true, SpotAvailable: true}
		if !ok || !ofs.Has(zoneID) {
			ret.Available = false
		}
		return ret
	})
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
func (p *DefaultProvider) createOfferings(_ context.Context, instanceType string, zones []ZoneData) []*cloudprovider.Offering {
	var offerings []*cloudprovider.Offering
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

func (p *DefaultProvider) createOffering(zone, capacityType string, price float64, available bool) *cloudprovider.Offering {
	return &cloudprovider.Offering{
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
	p.muInstanceTypesInfo.Lock()
	defer p.muInstanceTypesInfo.Unlock()

	types, err := p.getInstanceTypes(ctx)
	if err != nil {
		return fmt.Errorf("getting instance type offerings: %w", err)
	}

	newInstanceTypesOfferings := make(map[string]sets.Set[string])
	for _, mt := range types {
		ofs, ok := newInstanceTypesOfferings[*mt.Name]
		if !ok {
			ofs = sets.New[string]()
		}

		zone := *mt.Zone
		if lastSlash := strings.LastIndex(zone, "/"); lastSlash != -1 {
			zone = zone[lastSlash+1:]
		}
		newInstanceTypesOfferings[*mt.Name] = ofs.Insert(zone)
	}
	p.instanceTypesOfferings = newInstanceTypesOfferings

	log.FromContext(ctx).
		WithValues("newInstanceTypesOfferings count", len(newInstanceTypesOfferings)).
		Info("updated instance type offerings from aggregated list", "region", p.authOptions.Region)
	return nil
}

func (p *DefaultProvider) UpdateInstanceTypes(ctx context.Context) error {
	p.muInstanceTypesInfo.Lock()
	defer p.muInstanceTypesInfo.Unlock()

	types, err := p.getInstanceTypes(ctx)
	if err != nil {
		return fmt.Errorf("getting instance types: %w", err)
	}
	if p.cm.HasChanged("instance-types", types) {
		atomic.AddUint64(&p.instanceTypesSeqNum, 1)
		p.staticInstanceTypesCache.Flush()
	}
	p.instanceTypesInfo = types

	return nil
}

func (p *DefaultProvider) getInstanceTypes(ctx context.Context) ([]*computepb.MachineType, error) {
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

	return vmTypes, nil
}
