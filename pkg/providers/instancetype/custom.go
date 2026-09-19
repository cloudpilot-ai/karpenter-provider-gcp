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
	"net/http"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"google.golang.org/api/googleapi"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

// isMachineTypeNotFoundError reports whether err is a definitive "this machine type does not
// exist in this zone" response (HTTP 404), as opposed to a transient, authorization, quota, or
// context error, which says nothing about whether the shape is actually valid or available.
func isMachineTypeNotFoundError(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound
}

// getCachedCustomMachineType resolves a single custom machine type name against the given
// zones, caching the result to avoid re-querying GCP for the same name on every List call.
//
// Only a definitive not-found response (the shape is invalid for the family, or the family
// isn't offered in that zone) is treated as "not available in this zone". Any other error -
// transient, authorization, quota, or a canceled context - leaves that zone's availability
// unresolved and is logged rather than cached: caching it as unavailable would misreport a
// temporary API failure as the shape genuinely being absent, and would keep it unavailable
// for the cache TTL even after the API recovers. When any zone hit such an error, the whole
// result is left uncached (even zones that did resolve) so the next List call retries
// instead of settling for a possibly-incomplete zone set.
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
	sawUnresolvedZone := false
	for _, zone := range zones {
		mt, err := getMachineType(ctx, zone, name)
		switch {
		case err == nil && mt != nil:
			if machineType == nil {
				machineType = mt
			}
			availableZones.Insert(zone)
		case isMachineTypeNotFoundError(err):
			// Confirmed: not a valid/available shape in this zone.
		default:
			sawUnresolvedZone = true
			log.FromContext(ctx).Error(err, "failed to resolve custom machine type, leaving unresolved rather than caching as unavailable",
				"name", name, "zone", zone)
		}
	}
	if sawUnresolvedZone {
		return machineType, availableZones
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
// (vCPUs * perVCPUPrice) + (memoryMB * perMBPrice), with per-vCPU/per-MB rates approximately
// constant within a machine family. GCP does not publish prices for custom shapes directly
// (unlike aggregatedList/get, which do resolve custom shapes), so those rates are calibrated
// here via a least-squares fit over every predefined shape of the same family with a known
// price - standard/highmem/highcpu, at every size GCP offers - rather than just two shapes, so
// the estimate isn't skewed by a single noisy data point and works uniformly for any family,
// not only ones a test happens to cover.
func (p *DefaultProvider) deriveCustomPrice(mt *computepb.MachineType) (float64, bool) {
	family, _, ok := strings.Cut(lo.FromPtr(mt.Name), "-")
	if !ok {
		return 0, false
	}

	type pricePoint struct{ ratio, pricePerCPU float64 }
	var points []pricePoint
	distinctRatios := make(map[float64]struct{})
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
		distinctRatios[ratio] = struct{}{}
		points = append(points, pricePoint{ratio: ratio, pricePerCPU: price / float64(cpus)})
	}
	// A single memory-per-vCPU ratio (e.g. a family with only a "standard" shape known)
	// can't separate the per-vCPU and per-MB rates; need at least two distinct ratios.
	if len(distinctRatios) < 2 {
		return 0, false
	}

	// Ordinary least squares fit of pricePerCPU = perCPUPrice + perMBPrice*ratio.
	var n, sumRatio, sumPrice, sumRatioPrice, sumRatioSq float64
	for _, pt := range points {
		n++
		sumRatio += pt.ratio
		sumPrice += pt.pricePerCPU
		sumRatioPrice += pt.ratio * pt.pricePerCPU
		sumRatioSq += pt.ratio * pt.ratio
	}
	denominator := n*sumRatioSq - sumRatio*sumRatio
	if denominator == 0 {
		return 0, false
	}
	perMBPrice := (n*sumRatioPrice - sumRatio*sumPrice) / denominator
	perCPUPrice := (sumPrice - perMBPrice*sumRatio) / n

	price := perCPUPrice*float64(mt.GetGuestCpus()) + perMBPrice*float64(mt.GetMemoryMb())
	if price <= 0 {
		return 0, false
	}
	return price, true
}
