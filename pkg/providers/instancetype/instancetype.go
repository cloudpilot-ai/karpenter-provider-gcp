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
	"sync"
	"sync/atomic"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/aws/aws-sdk-go/aws"
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
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing"
)

const (
	InstanceTypesCacheKey = "gce-instancetypes"
	// InstanceTypesCacheTTL is the time before we refresh instance types and zones at GCE, not too long to avoid OOM
	InstanceTypesCacheTTL = 30 * time.Hour
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

	unavailableOfferings *pkgcache.UnavailableOfferings
	// FIXME: use cache to speed up query.
	instanceTypesCache *cache.Cache
}

func NewDefaultProvider(ctx context.Context, authOptions *auth.Credential, pricingProvider pricing.Provider,
	gkeProvider gke.Provider, unavailableOfferingsCache *pkgcache.UnavailableOfferings) *DefaultProvider {
	machineTypesClient, err := compute.NewMachineTypesRESTClient(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create default provider for node pool template")
		os.Exit(1)
	}
	return &DefaultProvider{
		authOptions:            authOptions,
		machineTypesClient:     machineTypesClient,
		pricingProvider:        pricingProvider,
		gkeProvider:            gkeProvider,
		instanceTypesCache:     cache.New(InstanceTypesCacheTTL, pkgcache.DefaultCleanupInterval),
		instanceTypesOfferings: make(map[string]sets.Set[string]),
		unavailableOfferings:   unavailableOfferingsCache,
		cm:                     pretty.NewChangeMonitor(),
		instanceTypesSeqNum:    0,
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

	zonesHash, _ := hashstructure.Hash(zones, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	kcHash, _ := hashstructure.Hash(nodeClass.Spec.KubeletConfiguration, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	listKey := fmt.Sprintf("%d-%d-%d-%d", p.instanceTypesSeqNum, p.unavailableOfferings.SeqNum, zonesHash, kcHash)
	item, ok := p.instanceTypesCache.Get(listKey)
	if ok && len(item.([]*cloudprovider.InstanceType)) > 0 {
		return item.([]*cloudprovider.InstanceType), nil
	}

	instanceTypes := []*cloudprovider.InstanceType{}
	for _, mt := range p.instanceTypesInfo {
		instanceType := aws.StringValue(mt.Name)
		if instanceType == "" || mt.MemoryMb == nil || mt.GuestCpus == nil {
			continue
		}

		// Make sure all zone is checked.
		zoneData := lo.Map(zones, func(zoneID string, _ int) ZoneData {
			// We assume that all zones are available for all instance types in spot.
			// Reference: https://cloud.google.com/compute/docs/instances/provisioning-models
			ret := ZoneData{ID: zoneID, Available: true, SpotAvailable: true}
			if !p.instanceTypesOfferings[instanceType].Has(zoneID) {
				ret.Available = false
			}
			return ret
		})
		offerings := p.createOfferings(ctx, instanceType, zoneData)
		if len(offerings) == 0 {
			continue
		}

		addInstanceType := NewInstanceType(ctx, mt, nodeClass, p.authOptions.Region, offerings)
		instanceTypes = append(instanceTypes, addInstanceType)
	}
	p.instanceTypesCache.SetDefault(listKey, instanceTypes)

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

		newInstanceTypesOfferings[*mt.Name] = ofs.Insert(*mt.Zone)
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
