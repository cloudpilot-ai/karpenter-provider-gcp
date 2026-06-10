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

// Package instanceprice provides GCP compute pricing using official Google APIs only.
// It combines the Cloud Billing Catalog API (per-unit prices) with the Compute
// Engine MachineTypes API (vCPU/RAM specs) to compute total per-machine hourly
// prices for both on-demand and spot capacity types.
package instanceprice

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/iterator"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/localssd"
)

const (
	// computeEngineServiceID is the stable Billing Catalog service ID for Compute Engine.
	computeEngineServiceID = "6F81-5844-456A"

	spotPreemptiblePrefix = "Spot Preemptible "

	// Billing API Category.UsageType values.
	usageTypeOnDemand    = "OnDemand"
	usageTypePreemptible = "Preemptible"

	componentCPU = "CPU"
	componentRAM = "RAM"

	// ramGByToGiBFactor converts GBy.h RAM prices to GiBy.h.
	// GCP expresses some RAM prices per SI gigabyte (GBy = 10^9 bytes); the Compute
	// API returns memory in MiB (binary), so we must multiply to obtain $/GiBy.
	// 1 GiB = 1.073741824 GB  →  price_per_GiB = price_per_GB × 1.073741824
	ramGByToGiBFactor = 1.073741824

	hoursPerMonth = 730.0
)

type FamilyUnitPrices struct {
	CPUOnDemandPerHour        float64 // $/vCPU-hour, on-demand
	RAMOnDemandPerGiB         float64 // $/GiB-hour, on-demand (normalised to GiB)
	CPUSpotPerHour            float64 // $/vCPU-hour, spot/preemptible; 0 if unavailable
	RAMSpotPerGiB             float64 // $/GiB-hour, spot/preemptible; 0 if unavailable
	CPUPremiumOnDemandPerHour float64 // $/vCPU-hour upgrade premium (m2 only); 0 for others
	RAMPremiumOnDemandPerGiB  float64 // $/GiB-hour upgrade premium (m2 only); 0 for others
	CPUPremiumSpotPerHour     float64 // $/vCPU-hour upgrade premium spot (m2 only); 0 for others
	RAMPremiumSpotPerGiB      float64 // $/GiB-hour upgrade premium spot (m2 only); 0 for others
}

type PerUnitPrice struct {
	OnDemandPerHour float64
	SpotPerHour     float64
}

type LocalSSDRate struct {
	OnDemandPerGiBHour float64 // $/GiB-hour, on-demand
	SpotPerGiBHour     float64 // $/GiB-hour, spot; 0 if unavailable
}

type RegionUnitPrices struct {
	Families              map[string]FamilyUnitPrices // keyed by machine family prefix (e.g. "n2", "e2")
	GPUs                  map[string]PerUnitPrice     // keyed by accelerator type (e.g. "nvidia-l4")
	FamilyLocalSSD        map[string]LocalSSDRate     // family-specific local SSD rates (c4, c4a, c4d); keyed by family prefix
	FlatRatePrices        map[string]PerUnitPrice     // flat per-instance hourly prices; keyed by machine type name (e.g. "e2-micro")
	SSDOnDemandPerGiBHour float64                     // generic local SSD on-demand price in $/GiB-hour
	SSDSpotPerGiBHour     float64                     // generic local SSD spot price in $/GiB-hour (0 if same as OD)
}

type AcceleratorSpec struct {
	Type  string // GCE accelerator type, e.g. "nvidia-tesla-a100"
	Count int32  // number of GPUs of this type
}

type MachineSpec struct {
	VCPUs        int32             // number of vCPUs
	MemoryMiB    int64             // RAM in mebibytes
	IsSharedCPU  bool              // true for shared-core (e2-micro, e2-small, e2-medium)
	LocalSSDGiB  float64           // total local SSD capacity in GiB (treating DiskGb as GiB, per billing); 0 if none
	Accelerators []AcceleratorSpec // attached GPUs; nil if none
}

type MachinePrice struct {
	Name            string
	Region          string
	OnDemandPerHour float64
	SpotPerHour     float64 // 0 when spot is not available for this family/region
}

// Client is safe for concurrent use after construction.
type Client struct {
	projectID          string
	billingService     *cloudbilling.APIService
	machineTypesClient *compute.MachineTypesClient
}

// New creates an instanceprice Client. If projectID is not provided, it is
// resolved from $GOOGLE_CLOUD_PROJECT, $GCLOUD_PROJECT, or Application
// Default Credentials in that order.
func New(ctx context.Context, projectID ...string) (*Client, error) {
	pid := ""
	if len(projectID) > 0 {
		pid = projectID[0]
	}
	if pid == "" {
		var err error
		pid, err = resolveProject(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolving GCP project: %w", err)
		}
	}

	billingSvc, err := cloudbilling.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating billing service: %w", err)
	}

	mtClient, err := compute.NewMachineTypesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating machine types client: %w", err)
	}

	return &Client{
		projectID:          pid,
		billingService:     billingSvc,
		machineTypesClient: mtClient,
	}, nil
}

// Close releases resources associated with the client.
func (c *Client) Close() {
	if err := c.machineTypesClient.Close(); err != nil {
		// Log/ignore the close error; Close is best-effort.
		_ = err
	}
}

// resolveProject resolves the GCP project ID from environment variables or ADC.
func resolveProject(ctx context.Context) (string, error) {
	for _, env := range []string{"GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT"} {
		if v := os.Getenv(env); v != "" {
			return v, nil
		}
	}
	if creds, err := google.FindDefaultCredentials(ctx); err == nil && creds.ProjectID != "" {
		return creds.ProjectID, nil
	}
	return "", fmt.Errorf("GCP project not found; set $GOOGLE_CLOUD_PROJECT or run: gcloud auth application-default login")
}

// The catalog is global (not region-specific); region filtering is applied
// client-side in ProcessSKUs. Results can be cached and reused across regions.
func (c *Client) FetchRawSKUs(ctx context.Context) ([]*cloudbilling.Sku, error) {
	var skus []*cloudbilling.Sku
	err := c.billingService.Services.Skus.
		List("services/"+computeEngineServiceID).
		CurrencyCode("USD").
		Pages(ctx, func(page *cloudbilling.ListSkusResponse) error {
			skus = append(skus, page.Skus...)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing billing SKUs: %w", err)
	}
	return skus, nil
}

func (c *Client) FetchUnitPrices(ctx context.Context, region string) (*RegionUnitPrices, error) {
	skus, err := c.FetchRawSKUs(ctx)
	if err != nil {
		return nil, err
	}
	return ProcessSKUs(skus, region), nil
}

// skus should be the full Compute Engine SKU catalog as returned by FetchRawSKUs.
//
// GCP can publish overlapping broad and region-specific SKUs for the same billing
// component. Handlers keep the higher applicable rate where this happens so the
// computed price does not understate the public regional price.
func ProcessSKUs(skus []*cloudbilling.Sku, region string) *RegionUnitPrices {
	out := &RegionUnitPrices{
		Families:       make(map[string]FamilyUnitPrices),
		GPUs:           make(map[string]PerUnitPrice),
		FamilyLocalSSD: make(map[string]LocalSSDRate),
		FlatRatePrices: make(map[string]PerUnitPrice),
	}
	for _, sku := range skus {
		usageType, price, ok := filterSKU(sku, region)
		if !ok {
			continue
		}

		desc := sku.Description
		if handlePremiumSKU(out, usageType, price, desc) {
			continue
		}
		if handleBaseSKU(out, usageType, price, desc) {
			continue
		}
		if handleGPUSKU(out, usageType, price, desc) {
			continue
		}
		if handleFlatRateSKU(out, usageType, price, desc) {
			continue
		}
		if handleFamilyLocalSSDSKU(out, usageType, price, desc) {
			continue
		}
		handleGenericSSDSKU(out, usageType, price, desc)
	}
	// m1 and m2 share the same base billing SKU prefix ("Memory-optimized Instance").
	// Due to Go map iteration randomness, only one of them will have matched the base
	// SKU during the loop above. Ensure both have the shared base prices.
	propagateSharedM1M2(out.Families)

	// Some GPU types have no on-demand regional SKU in reservation-only regions
	// (e.g. H100 Mega in us-central1/us-west1 — only Calendar Mode SKUs exist
	// there, which are filtered). Fill those gaps with the global minimum on-demand
	// rate so that machines using such GPUs still receive a meaningful price and
	// remain accessible to Karpenter (e.g. for reservation-based provisioning).
	fillMissingGPUPrices(out.GPUs, computeGlobalMinGPUPrices(skus))

	return out
}

type gpuMinPrices struct {
	onDemand map[string]float64
	spot     map[string]float64
}

// computeGlobalMinGPUPrices scans the full SKU catalog across all regions and
// returns the minimum on-demand and spot hourly price per GPU accelerator type.
func computeGlobalMinGPUPrices(skus []*cloudbilling.Sku) gpuMinPrices {
	mins := gpuMinPrices{
		onDemand: make(map[string]float64),
		spot:     make(map[string]float64),
	}
	for _, sku := range skus {
		accelType, usageType, price, ok := extractGlobalGPUPrice(sku)
		if !ok {
			continue
		}
		m := mins.onDemand
		if usageType == usageTypePreemptible {
			m = mins.spot
		}
		if cur, found := m[accelType]; !found || price < cur {
			m[accelType] = price
		}
	}
	return mins
}

func extractGlobalGPUPrice(sku *cloudbilling.Sku) (accelType, usageType string, price float64, ok bool) {
	if sku.Category == nil {
		return
	}
	usageType = sku.Category.UsageType
	if usageType != usageTypeOnDemand && usageType != usageTypePreemptible {
		return
	}
	desc := sku.Description
	if strings.Contains(desc, "DWS") ||
		strings.Contains(desc, "Calendar Mode") ||
		strings.Contains(desc, "Reserved") {
		return
	}
	var matched bool
	accelType, matched = matchByPrefix(desc, gpuSKUPrefixes)
	if !matched {
		return
	}
	var err error
	price, err = extractUnitPrice(sku)
	if err != nil || price == 0 {
		return
	}
	ok = true
	return
}

// fillMissingGPUPrices fills GPU types that have no regional SKU with the
// global minimum rate. Both on-demand and spot fields are filled independently,
// so accumulateGPUCosts uses realistic rates instead of falling back to the
// on-demand rate for spot.
func fillMissingGPUPrices(regional map[string]PerUnitPrice, globalMin gpuMinPrices) {
	for accelType, minOD := range globalMin.onDemand {
		gp := regional[accelType]
		if gp.OnDemandPerHour == 0 {
			gp.OnDemandPerHour = minOD
		}
		if gp.SpotPerHour == 0 {
			if minSpot, ok := globalMin.spot[accelType]; ok {
				gp.SpotPerHour = minSpot
			}
		}
		regional[accelType] = gp
	}
}

func filterSKU(sku *cloudbilling.Sku, region string) (string, float64, bool) {
	if !slices.Contains(sku.ServiceRegions, region) {
		return "", 0, false
	}
	if sku.Category == nil {
		return "", 0, false
	}
	usageType := sku.Category.UsageType
	if usageType != usageTypeOnDemand && usageType != usageTypePreemptible {
		return "", 0, false
	}

	price, err := extractUnitPrice(sku)
	// NOTE: price == 0 also filters out zero-priced base tiers from tiered SKUs
	// where extractUnitPrice returns the StartUsageAmount==0 tier. This coupling
	// is intentional — see extractUnitPrice comment about leading zero-priced tiers.
	if err != nil || price == 0 {
		return "", 0, false
	}
	return usageType, price, true
}

func handlePremiumSKU(out *RegionUnitPrices, usageType string, price float64, description string) bool {
	family, component, ok := matchPremiumSKU(description)
	if !ok {
		return false
	}
	up := out.Families[family]
	switch {
	case component == componentCPU && usageType == usageTypeOnDemand:
		up.CPUPremiumOnDemandPerHour = price
	case component == componentRAM && usageType == usageTypeOnDemand:
		up.RAMPremiumOnDemandPerGiB = price
	case component == componentCPU && usageType == usageTypePreemptible:
		up.CPUPremiumSpotPerHour = price
	case component == componentRAM && usageType == usageTypePreemptible:
		up.RAMPremiumSpotPerGiB = price
	}
	out.Families[family] = up
	return true
}

func handleBaseSKU(out *RegionUnitPrices, usageType string, price float64, description string) bool {
	family, component, ok := matchSKU(description)
	if !ok {
		return false
	}
	up := out.Families[family]
	setFamilyRate(&up, component, usageType, price)
	out.Families[family] = up
	return true
}

func setFamilyRate(up *FamilyUnitPrices, component, usageType string, price float64) {
	if component == componentCPU {
		if usageType == usageTypeOnDemand {
			up.CPUOnDemandPerHour = price
			return
		}
		if price > up.CPUSpotPerHour {
			up.CPUSpotPerHour = price
		}
		return
	}
	if usageType == usageTypeOnDemand {
		up.RAMOnDemandPerGiB = price
		return
	}
	if price > up.RAMSpotPerGiB {
		up.RAMSpotPerGiB = price
	}
}

func handleGPUSKU(out *RegionUnitPrices, usageType string, price float64, description string) bool {
	if strings.Contains(description, "DWS") ||
		strings.Contains(description, "Calendar Mode") ||
		strings.Contains(description, "Reserved") {
		return false
	}
	accelType, ok := matchByPrefix(description, gpuSKUPrefixes)
	if !ok {
		return false
	}
	gp := out.GPUs[accelType]
	setPerUnitRate(&gp, usageType, price, true)
	out.GPUs[accelType] = gp
	return true
}

func handleFlatRateSKU(out *RegionUnitPrices, usageType string, price float64, description string) bool {
	machineType, ok := matchByPrefix(strings.TrimPrefix(description, spotPreemptiblePrefix), flatRateSKUPrefixes)
	if !ok {
		return false
	}
	fp := out.FlatRatePrices[machineType]
	setPerUnitRate(&fp, usageType, price, false)
	out.FlatRatePrices[machineType] = fp
	return true
}

func setPerUnitRate(rate *PerUnitPrice, usageType string, price float64, preferHigher bool) {
	if usageType == usageTypeOnDemand {
		if preferHigher && price <= rate.OnDemandPerHour {
			return
		}
		rate.OnDemandPerHour = price
		return
	}
	if preferHigher && price <= rate.SpotPerHour {
		return
	}
	rate.SpotPerHour = price
}

func handleFamilyLocalSSDSKU(out *RegionUnitPrices, usageType string, price float64, description string) bool {
	family, ok := matchByPrefix(strings.TrimPrefix(description, spotPreemptiblePrefix), localSSDFamilySKUPrefixes)
	if !ok {
		return false
	}
	rate := out.FamilyLocalSSD[family]
	if usageType == usageTypeOnDemand {
		rate.OnDemandPerGiBHour = price
	} else {
		rate.SpotPerGiBHour = price
	}
	out.FamilyLocalSSD[family] = rate
	return true
}

func handleGenericSSDSKU(out *RegionUnitPrices, usageType string, price float64, description string) {
	if !isSSDSKU(description) {
		return
	}
	if usageType == usageTypeOnDemand {
		out.SSDOnDemandPerGiBHour = price
	} else {
		out.SSDSpotPerGiBHour = price
	}
}

// propagateSharedM1M2 ensures both m1 and m2 have the base CPU/RAM prices.
// They share a billing SKU prefix, so map iteration randomness may spread the four
// SKU components (OD CPU, OD RAM, Spot CPU, Spot RAM) across both map entries.
// M2's upgrade-premium prices are preserved separately.
func propagateSharedM1M2(families map[string]FamilyUnitPrices) {
	m1, m2 := families["m1"], families["m2"]
	// Use max for all rates: the shared SKU prefix means map iteration randomness
	// may spread components across m1 or m2. Taking the maximum ensures both
	// families use the most accurate regional rate regardless of iteration order.
	m2.CPUOnDemandPerHour = math.Max(m1.CPUOnDemandPerHour, m2.CPUOnDemandPerHour)
	m2.RAMOnDemandPerGiB = math.Max(m1.RAMOnDemandPerGiB, m2.RAMOnDemandPerGiB)
	m2.CPUSpotPerHour = math.Max(m1.CPUSpotPerHour, m2.CPUSpotPerHour)
	m2.RAMSpotPerGiB = math.Max(m1.RAMSpotPerGiB, m2.RAMSpotPerGiB)

	families["m1"] = FamilyUnitPrices{
		CPUOnDemandPerHour: m2.CPUOnDemandPerHour,
		RAMOnDemandPerGiB:  m2.RAMOnDemandPerGiB,
		CPUSpotPerHour:     m2.CPUSpotPerHour,
		RAMSpotPerGiB:      m2.RAMSpotPerGiB,
	}
	families["m2"] = m2
}

var localSSDRe = regexp.MustCompile(`(?i)(\d+)\s+local\s+ssd`)

// Machine types appear once per zone in the AggregatedList response; deduplicated by name.
func (c *Client) FetchMachineSpecs(ctx context.Context, region string) (map[string]MachineSpec, error) {
	filter := fmt.Sprintf("(zone eq .*%s-[a-z])", regexp.QuoteMeta(region))
	req := &computepb.AggregatedListMachineTypesRequest{
		Project: c.projectID,
		Filter:  &filter,
	}

	specs := make(map[string]MachineSpec)
	it := c.machineTypesClient.AggregatedList(ctx, req)
	for {
		pair, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing machine types: %w", err)
		}
		for _, mt := range pair.Value.MachineTypes {
			name, spec, ok := parseMachineType(mt)
			if !ok {
				continue
			}
			if _, seen := specs[name]; seen {
				continue // same spec in every zone; keep the first occurrence
			}

			specs[name] = spec
		}
	}
	return specs, nil
}

func parseMachineType(mt *computepb.MachineType) (string, MachineSpec, bool) {
	if mt == nil || mt.Name == nil || mt.GuestCpus == nil || mt.MemoryMb == nil {
		return "", MachineSpec{}, false
	}
	spec := MachineSpec{
		VCPUs:     *mt.GuestCpus,
		MemoryMiB: int64(*mt.MemoryMb),
	}
	if mt.IsSharedCpu != nil {
		spec.IsSharedCPU = *mt.IsSharedCpu
	}
	spec.LocalSSDGiB = resolveLocalSSDGiB(mt)
	spec.Accelerators = parseAccelerators(mt)

	return *mt.Name, spec, true
}

func resolveLocalSSDGiB(mt *computepb.MachineType) float64 {
	partitions := bundledPartitionCount(mt)
	name := mt.GetName()
	if name == "c4d-highmem-8-lssd" || name == "c4d-highmem-16-lssd" {
		return float64(partitions * int(localssd.DefaultPartitionGiB))
	}
	return float64(localssd.TotalGiB(name, partitions))
}

func bundledPartitionCount(mt *computepb.MachineType) int {
	if bls := mt.GetBundledLocalSsds(); bls != nil && bls.PartitionCount != nil && *bls.PartitionCount > 0 {
		return int(*bls.PartitionCount)
	}
	if mt.Description != nil {
		return parseLocalSSDCount(*mt.Description)
	}
	return 0
}

func parseAccelerators(mt *computepb.MachineType) []AcceleratorSpec {
	var accels []AcceleratorSpec
	for _, accel := range mt.Accelerators {
		if accel.GuestAcceleratorType == nil || accel.GuestAcceleratorCount == nil {
			continue
		}
		accels = append(accels, AcceleratorSpec{
			Type:  *accel.GuestAcceleratorType,
			Count: *accel.GuestAcceleratorCount,
		})
	}
	return accels
}

// AssemblePrices is the shared core of ComputePrices and ComputePricesFromSKUs.
func AssemblePrices(region string, regionPrices *RegionUnitPrices, specs map[string]MachineSpec) []MachinePrice {
	var prices []MachinePrice
	for name, spec := range specs {
		if IsBlacklisted(name) {
			continue
		}
		// Shared-core machines use non-standard billing.
		if spec.IsSharedCPU {
			if mp, ok := priceSharedCore(name, spec, regionPrices); ok {
				mp.Region = region
				prices = append(prices, mp)
			}
			continue
		}

		if mp, ok := priceStandardMachine(name, spec, regionPrices); ok {
			mp.Region = region
			prices = append(prices, mp)
		}
	}
	return prices
}

func priceSharedCore(name string, spec MachineSpec, regionPrices *RegionUnitPrices) (MachinePrice, bool) {
	mp := MachinePrice{Name: name}
	// E2 shared-core (e2-micro/small/medium): standard E2 per-vCPU/GiB rates
	// but billed on a fractional CPU count, not the full GuestCpus value.
	if cpuFrac, ok := sharedCoreCPUFraction[name]; ok {
		up, upOK := regionPrices.Families["e2"]
		if !upOK || (up.CPUOnDemandPerHour == 0 && up.RAMOnDemandPerGiB == 0) {
			return MachinePrice{}, false
		}
		ramGiB := float64(spec.MemoryMiB) / 1024.0
		mp.OnDemandPerHour = cpuFrac*up.CPUOnDemandPerHour + ramGiB*up.RAMOnDemandPerGiB
		if up.CPUSpotPerHour > 0 || up.RAMSpotPerGiB > 0 {
			mp.SpotPerHour = cpuFrac*up.CPUSpotPerHour + ramGiB*up.RAMSpotPerGiB
		}
		return mp, true
	}

	// f1-micro, g1-small: legacy flat per-instance rate.
	fp, ok := regionPrices.FlatRatePrices[name]
	if !ok || fp.OnDemandPerHour == 0 {
		return MachinePrice{}, false
	}
	mp.OnDemandPerHour = fp.OnDemandPerHour
	mp.SpotPerHour = fp.SpotPerHour
	return mp, true
}

func priceStandardMachine(name string, spec MachineSpec, regionPrices *RegionUnitPrices) (MachinePrice, bool) {
	family := resolveFamily(name)
	up, ok := regionPrices.Families[family]
	if !ok || (up.CPUOnDemandPerHour == 0 && up.RAMOnDemandPerGiB == 0) {
		return MachinePrice{}, false
	}

	ramGiB := float64(spec.MemoryMiB) / 1024.0
	vCPUs := float64(spec.VCPUs)

	gpuFraction := gpuBillingFraction[name]
	if gpuFraction == 0 {
		gpuFraction = 1.0
	}
	gpuODCost, gpuSpotCost := accumulateGPUCosts(spec.Accelerators, regionPrices, gpuFraction)
	totalSSDGiB := spec.LocalSSDGiB
	ssdODRate, ssdSpotRate := resolveLocalSSDRates(name, family, regionPrices)

	mp := MachinePrice{
		Name: name,
		OnDemandPerHour: vCPUs*up.CPUOnDemandPerHour + ramGiB*up.RAMOnDemandPerGiB +
			vCPUs*up.CPUPremiumOnDemandPerHour + ramGiB*up.RAMPremiumOnDemandPerGiB +
			gpuODCost + totalSSDGiB*ssdODRate,
	}

	// Guard on CPU/RAM spot rates: in the GCP Billing API no machine family
	// exposes preemptible GPU or local-SSD SKUs without also having preemptible
	// CPU/RAM SKUs — if CPU/RAM spot is absent the family does not support Spot
	// VMs at all and SpotPerHour should remain zero.
	//
	// Including gpuSpotCost or totalSSDGiB*ssdSpotRate unconditionally would
	// overstate spot prices for non-spot families: accumulateGPUCosts falls back
	// to the GPU on-demand rate when no GPU spot SKU exists, and
	// resolveLocalSSDRates falls back to the on-demand SSD rate — both are
	// always non-zero, so the gate must be CPU/RAM-based.
	if up.CPUSpotPerHour > 0 || up.RAMSpotPerGiB > 0 {
		mp.SpotPerHour = vCPUs*up.CPUSpotPerHour + ramGiB*up.RAMSpotPerGiB +
			vCPUs*up.CPUPremiumSpotPerHour + ramGiB*up.RAMPremiumSpotPerGiB +
			gpuSpotCost + totalSSDGiB*ssdSpotRate
	}

	return mp, true
}

// accumulateGPUCosts sums on-demand and spot GPU costs for the given accelerators.
// fraction scales the reported GuestAcceleratorCount to the billable count —
// use 1.0 for most machines; G4 sub-48 sizes have fractions < 1.
// Accelerator types with no billing SKU in this region contribute $0 — the machine
// is still included so Karpenter can attempt provisioning (e.g. via a reservation).
func accumulateGPUCosts(accels []AcceleratorSpec, regionPrices *RegionUnitPrices, fraction float64) (odCost, spotCost float64) {
	for _, accel := range accels {
		gp, ok := regionPrices.GPUs[accel.Type]
		if !ok {
			continue
		}
		count := float64(accel.Count) * fraction
		odCost += count * gp.OnDemandPerHour
		spotPerHour := gp.SpotPerHour
		if spotPerHour == 0 {
			spotPerHour = gp.OnDemandPerHour
		}
		spotCost += count * spotPerHour
	}
	return odCost, spotCost
}

func resolveLocalSSDRates(name, family string, regionPrices *RegionUnitPrices) (odRate, spotRate float64) {
	if famSSD, ok := regionPrices.FamilyLocalSSD[family]; ok {
		odRate = famSSD.OnDemandPerGiBHour
		spotRate = famSSD.SpotPerGiBHour
	} else if !strings.HasSuffix(name, "-lssd") || lssdGenericRateFamilies[family] {
		// Non-lssd machines: use generic rate for any attached SSD.
		// -lssd machines in lssdGenericRateFamilies (c3, c3d): billed via the generic
		// "SSD backed Local Storage" SKU rather than a family-specific one.
		// Other -lssd machines (c4/c4a/c4d/h4d) whose family SKU is absent in a region
		// have their SSD bundled in the VM price and contribute $0 additional cost.
		odRate = regionPrices.SSDOnDemandPerGiBHour
		spotRate = regionPrices.SSDSpotPerGiBHour
	}
	if spotRate == 0 {
		spotRate = odRate
	}
	return odRate, spotRate
}

// ComputePricesFromSKUs uses a pre-loaded SKU list; only FetchMachineSpecs hits the network.
// Avoids redundant Billing API calls when reusing a cached SKU list across regions.
func (c *Client) ComputePricesFromSKUs(ctx context.Context, skus []*cloudbilling.Sku, region string) ([]MachinePrice, error) {
	specs, err := c.FetchMachineSpecs(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("fetching machine specs: %w", err)
	}
	return AssemblePrices(region, ProcessSKUs(skus, region), specs), nil
}

// ComputePrices fetches both billing SKUs and machine specs concurrently and assembles prices.
//
// Both API calls run concurrently.
//
// Shared-core machine types (e2-micro, e2-small, e2-medium, f1-micro, g1-small)
// use flat per-instance billing; they are priced via FlatRatePrices.
// Machine types whose family is not in familyMatcherMap are omitted.
func (c *Client) ComputePrices(ctx context.Context, region string) ([]MachinePrice, error) {
	g, ctx := errgroup.WithContext(ctx)

	var skus []*cloudbilling.Sku
	g.Go(func() error {
		var err error
		skus, err = c.FetchRawSKUs(ctx)
		if err != nil {
			return fmt.Errorf("fetching billing SKUs: %w", err)
		}
		return nil
	})

	var specs map[string]MachineSpec
	g.Go(func() error {
		var err error
		specs, err = c.FetchMachineSpecs(ctx, region)
		if err != nil {
			return fmt.Errorf("fetching machine specs: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return AssemblePrices(region, ProcessSKUs(skus, region), specs), nil
}

// ExtractFamily is the first segment before the first "-":
//
//	"n2-standard-16"    → "n2"
//	"n2d-standard-8"    → "n2d"
//	"c3d-standard-8"    → "c3d"
//	"n2-custom-4-16384" → "n2"
//
// Exported for use in tests and by the pricing provider.
func ExtractFamily(machineTypeName string) string {
	prefix, _, found := strings.Cut(machineTypeName, "-")
	if found {
		return prefix
	}
	return machineTypeName
}

// resolveFamily checks machineNameFamilyPrefixOverrides first (for machines whose name prefix
// does not match their billing SKU family), then falls back to ExtractFamily.
func resolveFamily(machineTypeName string) string {
	for _, ov := range machineNameFamilyPrefixOverrides {
		if strings.HasPrefix(machineTypeName, ov.prefix) {
			return ov.familyKey
		}
	}
	return ExtractFamily(machineTypeName)
}

// MatchSKU is exported for use in tests.
func MatchSKU(description string) (family, component string, ok bool) {
	return matchSKU(description)
}

// ExtractUnitPrice is exported for use in tests.
func ExtractUnitPrice(sku *cloudbilling.Sku) (float64, error) {
	return extractUnitPrice(sku)
}

// ResolveFamily is exported for use in tests.
func ResolveFamily(machineTypeName string) string {
	return resolveFamily(machineTypeName)
}

// RamGByToGiBFactor is exported for use in tests.
const RamGByToGiBFactor = ramGByToGiBFactor

// HoursPerMonth is exported for use in tests.
const HoursPerMonth = hoursPerMonth

// matchSKU strips the "Spot Preemptible " prefix first so one familyMatchers entry
// covers both on-demand and spot SKUs. Iteration order is deterministic.
func matchSKU(description string) (family, component string, ok bool) {
	desc := strings.TrimPrefix(description, spotPreemptiblePrefix)
	for _, e := range familyMatchers {
		if strings.HasPrefix(desc, e.cpuPrefix) || (e.cpuAltPrefix != "" && strings.HasPrefix(desc, e.cpuAltPrefix)) {
			return e.key, componentCPU, true
		}
		if strings.HasPrefix(desc, e.ramPrefix) || (e.ramAltPrefix != "" && strings.HasPrefix(desc, e.ramAltPrefix)) {
			return e.key, componentRAM, true
		}
	}
	return "", "", false
}

// matchPremiumSKU handles upgrade-premium SKUs, e.g. "m2 Memory Optimized Upgrade". Iteration order is deterministic.
func matchPremiumSKU(description string) (family, component string, ok bool) {
	desc := strings.TrimPrefix(description, spotPreemptiblePrefix)
	for _, e := range familyMatchers {
		if e.cpuPremiumPrefix != "" && strings.HasPrefix(desc, e.cpuPremiumPrefix) {
			return e.key, componentCPU, true
		}
		if e.ramPremiumPrefix != "" && strings.HasPrefix(desc, e.ramPremiumPrefix) {
			return e.key, componentRAM, true
		}
	}
	return "", "", false
}

// matchByPrefix iterates in deterministic slice order.
func matchByPrefix(description string, prefixes []prefixEntry) (string, bool) {
	for _, e := range prefixes {
		if strings.HasPrefix(description, e.prefix) {
			return e.key, true
		}
	}
	return "", false
}

func isSSDSKU(description string) bool {
	return strings.HasPrefix(description, "SSD backed Local Storage")
}

// parseLocalSSDCount extracts the number of local SSD partitions from a
// MachineType Description string (e.g. "4 vCPUs, 16 GB RAM, 2 local SSDs" → 2).
func parseLocalSSDCount(description string) int {
	m := localSSDRe.FindStringSubmatch(description)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// extractUnitPrice converts a billing SKU's base tiered rate into a
// float64 price per usage unit per hour.
//
// Unit conversions applied:
//   - GBy.h  → multiplied by ramGByToGiBFactor to normalise to GiBy.h
//   - GiBy.mo → divided by hoursPerMonth to convert to GiBy.h
func extractUnitPrice(sku *cloudbilling.Sku) (float64, error) {
	if len(sku.PricingInfo) == 0 {
		return 0, fmt.Errorf("SKU %q has no pricing info", sku.Description)
	}
	expr := sku.PricingInfo[0].PricingExpression
	if expr == nil || len(expr.TieredRates) == 0 {
		return 0, fmt.Errorf("SKU %q has no tiered rates", sku.Description)
	}
	// GCP orders TieredRates by StartUsageAmount. Use the rate at StartUsageAmount == 0
	// (the base price). Some SKUs have a leading zero-priced tier; explicitly finding
	// StartUsageAmount == 0 avoids accidentally picking such a tier.
	var rate *cloudbilling.TierRate
	for _, r := range expr.TieredRates {
		if r.StartUsageAmount == 0 {
			rate = r
			break
		}
	}
	if rate == nil {
		rate = expr.TieredRates[0]
	}
	if rate.UnitPrice == nil {
		return 0, fmt.Errorf("SKU %q tiered rate has no unit price", sku.Description)
	}

	price := float64(rate.UnitPrice.Units) + float64(rate.UnitPrice.Nanos)*1e-9

	switch expr.UsageUnit {
	case "GBy.h":
		price *= ramGByToGiBFactor
	case "GiBy.mo":
		price /= hoursPerMonth
	}

	return price, nil
}

// Prices holds hourly on-demand and spot prices for machine types in a region.
type Prices struct {
	OnDemand map[string]float64 `json:"on_demand,omitempty"`
	Spot     map[string]float64 `json:"spot,omitempty"`
}

// FetchRegionPrices returns computed prices for a single GCP region.
func (c *Client) FetchRegionPrices(ctx context.Context, region string) (Prices, error) {
	machPrices, err := c.ComputePrices(ctx, region)
	if err != nil {
		return Prices{}, err
	}
	od := make(map[string]float64, len(machPrices))
	spot := make(map[string]float64)
	for _, mp := range machPrices {
		od[mp.Name] = mp.OnDemandPerHour
		if mp.SpotPerHour > 0 {
			spot[mp.Name] = mp.SpotPerHour
		}
	}
	return Prices{OnDemand: od, Spot: spot}, nil
}

// FetchRegions returns all GCP region names for the client's project.
func (c *Client) FetchRegions(ctx context.Context) ([]string, error) {
	regionsClient, err := compute.NewRegionsRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating regions client: %w", err)
	}
	defer regionsClient.Close()

	it := regionsClient.List(ctx, &computepb.ListRegionsRequest{Project: c.projectID})
	var regions []string
	for {
		region, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing regions: %w", err)
		}
		regions = append(regions, region.GetName())
	}
	slices.Sort(regions)
	return regions, nil
}

// FetchAllPrices returns computed prices for all GCP regions.
// The Billing SKU catalog (global, identical for every region) is fetched once
// and shared across all per-region goroutines via ComputePricesFromSKUs, which
// only hits the Compute Engine MachineTypes API per region.
// The SKU fetch and region list fetch run concurrently.
func (c *Client) FetchAllPrices(ctx context.Context) (map[string]Prices, error) {
	g, ctx := errgroup.WithContext(ctx)

	var skus []*cloudbilling.Sku
	g.Go(func() error {
		var err error
		skus, err = c.FetchRawSKUs(ctx)
		if err != nil {
			return fmt.Errorf("fetching billing SKUs: %w", err)
		}
		return nil
	})

	var regions []string
	g.Go(func() error {
		var err error
		regions, err = c.FetchRegions(ctx)
		if err != nil {
			return fmt.Errorf("listing regions: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	out := make(map[string]Prices, len(regions))
	var mu sync.Mutex
	g, ctx = errgroup.WithContext(ctx)
	for _, r := range regions {
		region := r
		g.Go(func() error {
			machPrices, err := c.ComputePricesFromSKUs(ctx, skus, region)
			if err != nil {
				return fmt.Errorf("region %s: %w", region, err)
			}
			od := make(map[string]float64, len(machPrices))
			spot := make(map[string]float64)
			for _, mp := range machPrices {
				od[mp.Name] = mp.OnDemandPerHour
				if mp.SpotPerHour > 0 {
					spot[mp.Name] = mp.SpotPerHour
				}
			}
			mu.Lock()
			out[region] = Prices{OnDemand: od, Spot: spot}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
