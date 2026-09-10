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
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	// customMachineTypeCacheTTL bounds how long a resolved custom machine type (and the
	// zones it was found available in) is cached before being re-verified against GCP.
	customMachineTypeCacheTTL = time.Hour
	// customMachineTypeNegativeCacheTTL bounds how long a name that failed resolution in
	// every zone (invalid shape, or a family that doesn't support custom shapes there) is
	// cached, so a NodePool requesting a bad name doesn't hit the API on every List call.
	customMachineTypeNegativeCacheTTL = 5 * time.Minute
)

// customMachineTypeNameRe matches GCE custom machine type names, e.g. "n2-custom-8-24576"
// or the extended-memory form "n2-custom-8-245760-ext".
var customMachineTypeNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*-custom-[0-9]+-[0-9]+(-ext)?$`)

func isCustomMachineTypeName(name string) bool {
	return customMachineTypeNameRe.MatchString(name)
}

type customMachineTypeCacheEntry struct {
	machineType *computepb.MachineType
	zones       sets.Set[string]
}

// resolveCustomMachineTypes looks up GCE custom machine types (e.g. n2-custom-8-24576)
// named in NodePool/NodeClaim instance-type requirements.
//
// Custom machine types are parametric shapes, not catalog entries: the machineTypes.
// aggregatedList API used to populate the regular instance type catalog never returns
// them, even when instances using that exact shape are already running in the project
// (see docs/troubleshooting.md, "Custom machine types not discovered"). A NodePool
// pinned to a custom shape would otherwise have zero compatible instance types.
//
// machineTypes.get, unlike aggregatedList, does resolve a specific valid custom shape on
// demand without requiring an existing instance, so requested custom names that aren't
// already in the catalog are looked up individually and merged in for this List call.
func resolveCustomMachineTypes(
	ctx context.Context,
	requestedNames, zones []string,
	known []*computepb.MachineType,
	cacheStore *cache.Cache,
	getMachineType func(ctx context.Context, zone, name string) (*computepb.MachineType, error),
) ([]*computepb.MachineType, map[string]sets.Set[string]) {
	knownNames := sets.New[string]()
	for _, mt := range known {
		knownNames.Insert(lo.FromPtr(mt.Name))
	}

	var machineTypes []*computepb.MachineType
	offerings := make(map[string]sets.Set[string])
	for _, name := range lo.Uniq(requestedNames) {
		if name == "" || knownNames.Has(name) || !isCustomMachineTypeName(name) {
			continue
		}

		mt, availableZones := getCachedCustomMachineType(ctx, name, zones, cacheStore, getMachineType)
		if mt == nil || availableZones.Len() == 0 {
			continue
		}
		machineTypes = append(machineTypes, mt)
		offerings[name] = availableZones
	}
	return machineTypes, offerings
}

// getCachedCustomMachineType resolves a single custom machine type name against the
// given zones, caching the result (including a negative result, briefly) to avoid
// re-querying GCP for the same name on every List call.
func getCachedCustomMachineType(
	ctx context.Context,
	name string,
	zones []string,
	cacheStore *cache.Cache,
	getMachineType func(ctx context.Context, zone, name string) (*computepb.MachineType, error),
) (*computepb.MachineType, sets.Set[string]) {
	if cached, ok := cacheStore.Get(name); ok {
		entry := cached.(customMachineTypeCacheEntry)
		return entry.machineType, entry.zones
	}

	var machineType *computepb.MachineType
	availableZones := sets.New[string]()
	for _, zone := range zones {
		mt, err := getMachineType(ctx, zone, name)
		if err != nil || mt == nil {
			// Not available in this zone (e.g. the family isn't offered there), or the
			// requested shape is invalid for the family - either way, just skip it.
			continue
		}
		if machineType == nil {
			machineType = mt
		}
		availableZones.Insert(zone)
	}

	ttl := customMachineTypeCacheTTL
	if machineType == nil {
		ttl = customMachineTypeNegativeCacheTTL
	}
	cacheStore.Set(name, customMachineTypeCacheEntry{machineType: machineType, zones: availableZones}, ttl)
	return machineType, availableZones
}

// deriveCustomPrice estimates the on-demand hourly price of a custom machine type using GCP's
// linear vCPU + memory pricing model: predefined machine types bill as
// (vCPUs * perVCPUPrice) + (memoryMB * perMBPrice), with per-vCPU/per-MB rates constant within
// a machine family. GCP does not publish prices for custom shapes directly (unlike
// aggregatedList/get, which do resolve custom shapes), so those rates are calibrated here from
// two predefined shapes of the same family with different memory-per-vCPU ratios (e.g.
// standard/highmem) whose prices are already known.
func (p *DefaultProvider) deriveCustomPrice(mt *computepb.MachineType) (float64, bool) {
	family, _, ok := strings.Cut(lo.FromPtr(mt.Name), "-")
	if !ok {
		return 0, false
	}

	type pricePoint struct{ ratio, pricePerCPU float64 }
	seenRatios := make(map[float64]struct{})
	var points []pricePoint
	for _, sibling := range p.instanceTypesInfo {
		siblingName := lo.FromPtr(sibling.Name)
		siblingFamily, _, _ := strings.Cut(siblingName, "-")
		if siblingFamily != family || isCustomMachineTypeName(siblingName) {
			continue
		}
		cpus := sibling.GetGuestCpus()
		if cpus == 0 {
			continue
		}
		price, ok := p.pricingProvider.OnDemandPrice(siblingName)
		if !ok {
			continue
		}
		ratio := float64(sibling.GetMemoryMb()) / float64(cpus)
		if _, dup := seenRatios[ratio]; dup {
			continue
		}
		seenRatios[ratio] = struct{}{}
		points = append(points, pricePoint{ratio: ratio, pricePerCPU: price / float64(cpus)})
	}
	if len(points) < 2 {
		return 0, false
	}
	sort.Slice(points, func(i, j int) bool { return points[i].ratio < points[j].ratio })

	lowest, highest := points[0], points[len(points)-1]
	if highest.ratio == lowest.ratio {
		return 0, false
	}

	perMBPrice := (highest.pricePerCPU - lowest.pricePerCPU) / (highest.ratio - lowest.ratio)
	perCPUPrice := lowest.pricePerCPU - perMBPrice*lowest.ratio

	price := perCPUPrice*float64(mt.GetGuestCpus()) + perMBPrice*float64(mt.GetMemoryMb())
	if price <= 0 {
		return 0, false
	}
	return price, true
}
