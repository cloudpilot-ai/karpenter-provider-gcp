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

package instanceprice_test

import (
	"math"
	"testing"

	"google.golang.org/api/cloudbilling/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing/instanceprice"
)

const (
	n2CPUPriceNanos = 31611000 // $0.031611/h
	n2RAMPriceNanos = 4237000  // $0.004237/GBy.h
)

func makeSKU(desc, usageType string, nanos int64, regions ...string) *cloudbilling.Sku {
	const defaultRegion = "us-central1"
	if len(regions) == 0 {
		regions = []string{defaultRegion}
	}
	return &cloudbilling.Sku{
		Description:    desc,
		ServiceRegions: regions,
		Category:       &cloudbilling.Category{UsageType: usageType},
		PricingInfo: []*cloudbilling.PricingInfo{
			{
				PricingExpression: &cloudbilling.PricingExpression{
					UsageUnit: "h",
					TieredRates: []*cloudbilling.TierRate{
						{UnitPrice: &cloudbilling.Money{Nanos: nanos}},
					},
				},
			},
		},
	}
}

func TestMatchSKU(t *testing.T) {
	tests := []struct {
		desc          string
		wantFamily    string
		wantComponent string
		wantOK        bool
	}{
		// On-demand CPU
		{"N2 Instance Core running in Americas", "n2", "CPU", true},
		// On-demand RAM
		{"N2 Instance Ram running in Americas", "n2", "RAM", true},
		// Spot/preemptible CPU — prefix stripped before matching
		{"Spot Preemptible N2 Instance Core running in Americas", "n2", "CPU", true},
		// Spot/preemptible RAM
		{"Spot Preemptible N2 Instance Ram running in Americas", "n2", "RAM", true},
		// E2
		{"E2 Instance Core running in Americas", "e2", "CPU", true},
		{"E2 Instance Ram running in Americas", "e2", "RAM", true},
		// C2 uses "Compute optimized" billing family (note: no "Instance" in the SKU description)
		{"Compute optimized Core running in Americas", "c2", "CPU", true},
		{"Spot Preemptible Compute optimized Ram running in Americas", "c2", "RAM", true},
		// M1/M2 share "Memory-optimized Instance" prefix
		{"Memory-optimized Instance Core running in Americas", "m1", "CPU", true},
		// N1 predefined
		{"N1 Predefined Instance Core running in Americas", "n1", "CPU", true},
		{"N1 Predefined Instance Ram running in Americas", "n1", "RAM", true},
		// Arm families
		{"T2A Arm Instance Core running in Americas", "t2a", "CPU", true},
		{"C4A Arm Instance Core running in Americas", "c4a", "CPU", true},
		// Unknown description — no match
		{"Some Unknown SKU Description", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			fam, comp, ok := instanceprice.MatchSKU(tt.desc)
			if ok != tt.wantOK {
				t.Fatalf("MatchSKU(%q) ok=%v, want %v", tt.desc, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if comp != tt.wantComponent {
				t.Errorf("MatchSKU(%q) component=%q, want %q", tt.desc, comp, tt.wantComponent)
			}
			// m1 and m2 share the same SKU prefix; we only check that the returned
			// family is one of the two when the prefix is "Memory-optimized".
			if fam != tt.wantFamily && (tt.wantFamily != "m1" || fam != "m2") {
				t.Errorf("MatchSKU(%q) family=%q, want %q", tt.desc, fam, tt.wantFamily)
			}
		})
	}
}

func TestExtractUnitPrice(t *testing.T) {
	makeSKULocal := func(desc, usageUnit string, units, nanos int64) *cloudbilling.Sku {
		return &cloudbilling.Sku{
			Description: desc,
			PricingInfo: []*cloudbilling.PricingInfo{
				{
					PricingExpression: &cloudbilling.PricingExpression{
						UsageUnit: usageUnit,
						TieredRates: []*cloudbilling.TierRate{
							{
								UnitPrice: &cloudbilling.Money{
									Units: units,
									Nanos: nanos,
								},
							},
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name      string
		sku       *cloudbilling.Sku
		wantPrice float64
		wantErr   bool
	}{
		{
			name:      "cpu h unit",
			sku:       makeSKULocal("N2 Instance Core", "h", 0, n2CPUPriceNanos),
			wantPrice: 0.031611,
		},
		{
			name:      "whole units and nanos",
			sku:       makeSKULocal("N2 Instance Core", "h", 1, 500000000), // $1.5/h
			wantPrice: 1.5,
		},
		{
			name: "ram GBy.h normalised to GiBy.h",
			// $0.004237/GBy.h * 1.073741824 ≈ 0.004549
			sku:       makeSKULocal("N2 Instance Ram", "GBy.h", 0, n2RAMPriceNanos),
			wantPrice: 0.004237 * instanceprice.RamGByToGiBFactor,
		},
		{
			name: "local SSD GiBy.mo converted to GiBy.h",
			// $0.080/GiBy.mo / 730 ≈ $0.0001095890/GiBy.h
			sku:       makeSKULocal("SSD backed Local Storage", "GiBy.mo", 0, 80000000),
			wantPrice: 0.080 / instanceprice.HoursPerMonth,
		},
		{
			name:    "no pricing info",
			sku:     &cloudbilling.Sku{Description: "Bad SKU"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := instanceprice.ExtractUnitPrice(tt.sku)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExtractUnitPrice() err=%v, wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			const epsilon = 1e-9
			if diff := got - tt.wantPrice; diff > epsilon || diff < -epsilon {
				t.Errorf("ExtractUnitPrice() = %v, want %v", got, tt.wantPrice)
			}
		})
	}
}

func TestProcessSKUs(t *testing.T) {
	const region = "us-central1"

	prices := instanceprice.ProcessSKUs([]*cloudbilling.Sku{
		makeSKU("N2 Instance Core running in Americas", "OnDemand", n2CPUPriceNanos),
		makeSKU("N2 Instance Ram running in Americas", "OnDemand", n2RAMPriceNanos),
		makeSKU("Spot Preemptible N2 Instance Core running in Americas", "Preemptible", 9500000),
		makeSKU("Spot Preemptible N2 Instance Ram running in Americas", "Preemptible", 1000000),
		makeSKU("Memory-optimized Instance Core running in Americas", "OnDemand", 100000000),
		makeSKU("Memory-optimized Instance Ram running in Americas", "OnDemand", 10000000),
		makeSKU("Memory Optimized Upgrade Premium for Memory-optimized Instance Core running in Americas", "OnDemand", 50000000),
		makeSKU("N2 Instance Core running in Europe", "OnDemand", 99999999, "europe-west1"),
		{Description: "Bad SKU", ServiceRegions: []string{region}},
	}, region)

	n2 := prices.Families["n2"]
	if n2.CPUOnDemandPerHour == 0 || n2.RAMOnDemandPerGiB == 0 || n2.CPUSpotPerHour == 0 || n2.RAMSpotPerGiB == 0 {
		t.Fatalf("n2 prices incomplete: %+v", n2)
	}
	if n2.CPUOnDemandPerHour >= 0.099 {
		t.Errorf("n2 CPU price looks contaminated by wrong-region SKU: %v", n2.CPUOnDemandPerHour)
	}
	if prices.Families["m1"].CPUOnDemandPerHour != prices.Families["m2"].CPUOnDemandPerHour {
		t.Errorf("m1/m2 shared base CPU prices differ: m1=%+v m2=%+v", prices.Families["m1"], prices.Families["m2"])
	}
	if prices.Families["m2"].CPUPremiumOnDemandPerHour == 0 || prices.Families["m1"].CPUPremiumOnDemandPerHour != 0 {
		t.Errorf("m2 premium propagation wrong: m1=%+v m2=%+v", prices.Families["m1"], prices.Families["m2"])
	}
}

func TestProcessSKUs_PreferHigherSpotRate(t *testing.T) {
	const region = "us-central1"

	skus := []*cloudbilling.Sku{
		makeSKU("N2 Instance Core running in Americas", "OnDemand", n2CPUPriceNanos),
		makeSKU("N2 Instance Ram running in Americas", "OnDemand", n2RAMPriceNanos),
		// Two spot CPU SKUs: broad regional (lower) and region-specific (higher).
		makeSKU("Spot Preemptible N2 Instance Core running in Americas", "Preemptible", 5000000),
		makeSKU("Spot Preemptible N2 Instance Core running in US Central 1", "Preemptible", 9500000),
		// Two spot RAM SKUs: broad regional (lower) and region-specific (higher).
		makeSKU("Spot Preemptible N2 Instance Ram running in Americas", "Preemptible", 500000),
		makeSKU("Spot Preemptible N2 Instance Ram running in US Central 1", "Preemptible", 1000000),
	}

	out := instanceprice.ProcessSKUs(skus, region)
	n2 := out.Families["n2"]

	// Must pick the higher (region-specific) spot rate, not the lower (broad) rate.
	wantCPUSpot := 0.0095
	if math.Abs(n2.CPUSpotPerHour-wantCPUSpot) > 1e-9 {
		t.Errorf("CPUSpotPerHour = %v, want %v (should prefer higher rate)", n2.CPUSpotPerHour, wantCPUSpot)
	}
	wantRAMSpot := 0.001 // h unit — no GBy→GiB conversion
	if math.Abs(n2.RAMSpotPerGiB-wantRAMSpot) > 1e-9 {
		t.Errorf("RAMSpotPerGiB = %v, want %v (should prefer higher rate)", n2.RAMSpotPerGiB, wantRAMSpot)
	}
}

func TestResolveFamily(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"a3-megagpu-8g", "a3plus"},          // override: a3-megagpu* → a3plus (A3Plus billing SKUs)
		{"a3-ultragpu-8g", "a3ultra"},        // override: a3-ultra* → a3ultra
		{"m4-ultramem-224", "m4ultramem224"}, // override: m4-ultramem-224 → m4ultramem224
		{"m4-ultramem-112", "m4"},            // NOT overridden: 56/112 use standard M4
		{"n2-standard-8", "n2"},              // standard fallback via ExtractFamily
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := instanceprice.ResolveFamily(tt.input); got != tt.want {
				t.Errorf("ResolveFamily(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func assertPrice(t *testing.T, price instanceprice.MachinePrice, wantOD, wantSpot float64) {
	t.Helper()
	if math.Abs(price.OnDemandPerHour-wantOD) > 1e-9 {
		t.Errorf("OnDemandPerHour = %v, want %v", price.OnDemandPerHour, wantOD)
	}
	if math.Abs(price.SpotPerHour-wantSpot) > 1e-9 {
		t.Errorf("SpotPerHour = %v, want %v", price.SpotPerHour, wantSpot)
	}
}

func pricesByName(prices []instanceprice.MachinePrice) map[string]instanceprice.MachinePrice {
	out := map[string]instanceprice.MachinePrice{}
	for _, price := range prices {
		out[price.Name] = price
	}
	return out
}

func TestAssemblePrices_CPUAndSpot(t *testing.T) {
	rp := &instanceprice.RegionUnitPrices{Families: map[string]instanceprice.FamilyUnitPrices{
		"n2": {CPUOnDemandPerHour: 0.50, RAMOnDemandPerGiB: 0.10, CPUSpotPerHour: 0.15, RAMSpotPerGiB: 0.03},
	}}
	specs := map[string]instanceprice.MachineSpec{"n2-standard-2": {VCPUs: 2, MemoryMiB: 8192}}
	prices := instanceprice.AssemblePrices("us-central1", rp, specs)
	if len(prices) != 1 {
		t.Fatalf("got %d prices, want 1", len(prices))
	}
	assertPrice(t, prices[0], 1.80, 0.54)
}

func TestAssemblePrices_SharedCoreAndFlatRate(t *testing.T) {
	rp := &instanceprice.RegionUnitPrices{
		Families: map[string]instanceprice.FamilyUnitPrices{
			"e2": {CPUOnDemandPerHour: 1.00, RAMOnDemandPerGiB: 0.20, CPUSpotPerHour: 0.30, RAMSpotPerGiB: 0.06},
		},
		FlatRatePrices: map[string]instanceprice.PerUnitPrice{
			"f1-micro": {OnDemandPerHour: 0.0076, SpotPerHour: 0.0021},
		},
	}
	specs := map[string]instanceprice.MachineSpec{
		"e2-micro": {VCPUs: 2, MemoryMiB: 1024, IsSharedCPU: true},
		"f1-micro": {VCPUs: 1, MemoryMiB: 614, IsSharedCPU: true},
	}
	prices := pricesByName(instanceprice.AssemblePrices("us-central1", rp, specs))
	assertPrice(t, prices["e2-micro"], 0.45, 0.135)
	assertPrice(t, prices["f1-micro"], 0.0076, 0.0021)
}

func TestAssemblePrices_GPUFractionAndMissingGPUSKU(t *testing.T) {
	rp := &instanceprice.RegionUnitPrices{
		Families: map[string]instanceprice.FamilyUnitPrices{
			"g4":     {CPUOnDemandPerHour: 0.04890, RAMOnDemandPerGiB: 0.00587, CPUSpotPerHour: 0.00978, RAMSpotPerGiB: 0.001176},
			"a3plus": {CPUOnDemandPerHour: 0.05, RAMOnDemandPerGiB: 0.006, CPUSpotPerHour: 0.01, RAMSpotPerGiB: 0.001},
		},
		GPUs: map[string]instanceprice.PerUnitPrice{
			"nvidia-rtx-pro-6000": {OnDemandPerHour: 1.09565, SpotPerHour: 0.21910},
		},
	}
	specs := map[string]instanceprice.MachineSpec{
		"g4-standard-6": {VCPUs: 6, MemoryMiB: 22528, Accelerators: []instanceprice.AcceleratorSpec{{Type: "nvidia-rtx-pro-6000", Count: 1}}},
		"a3-megagpu-8g": {VCPUs: 208, MemoryMiB: 1949696, Accelerators: []instanceprice.AcceleratorSpec{{Type: "nvidia-h100-mega-80gb", Count: 8}}},
	}
	prices := pricesByName(instanceprice.AssemblePrices("us-central1", rp, specs))
	assertPrice(t, prices["g4-standard-6"], 0.55949625, 0.1119395)
	assertPrice(t, prices["a3-megagpu-8g"], 21.824, 3.984)
}

func TestAssemblePrices_LocalSSDRates(t *testing.T) {
	rp := &instanceprice.RegionUnitPrices{
		Families: map[string]instanceprice.FamilyUnitPrices{
			"c3": {CPUOnDemandPerHour: 0.04, RAMOnDemandPerGiB: 0.005},
			"c4": {CPUOnDemandPerHour: 0.04, RAMOnDemandPerGiB: 0.005},
		},
		FamilyLocalSSD:        map[string]instanceprice.LocalSSDRate{"c4": {OnDemandPerGiBHour: 0.0001}},
		SSDOnDemandPerGiBHour: 0.0001,
	}
	specs := map[string]instanceprice.MachineSpec{
		"c4-standard-8-lssd": {VCPUs: 8, MemoryMiB: 30720, LocalSSDGiB: 375},
		"c3-standard-8-lssd": {VCPUs: 8, MemoryMiB: 32768, LocalSSDGiB: 750},
	}
	prices := pricesByName(instanceprice.AssemblePrices("us-central1", rp, specs))
	assertPrice(t, prices["c4-standard-8-lssd"], 0.5075, 0)
	assertPrice(t, prices["c3-standard-8-lssd"], 0.555, 0)
}

func TestAssemblePrices_MissingFamilySkipped(t *testing.T) {
	rp := &instanceprice.RegionUnitPrices{Families: map[string]instanceprice.FamilyUnitPrices{"n2": {CPUOnDemandPerHour: 0.50, RAMOnDemandPerGiB: 0.10}}}
	specs := map[string]instanceprice.MachineSpec{"unknown-type-4": {VCPUs: 4, MemoryMiB: 16384}}
	if prices := instanceprice.AssemblePrices("us-central1", rp, specs); len(prices) != 0 {
		t.Errorf("got %d prices, want 0", len(prices))
	}
}

func TestIsBlacklisted(t *testing.T) {
	blacklisted := []string{
		"z3-highmem-88",
		"z3-highmem-176",
		"n1-megamem-96",
		"n1-ultramem-40",
		"n1-ultramem-80",
		"n1-ultramem-160",
		"m2-ultramem2x-96",
		"m2-ultramemx-96",
	}
	for _, name := range blacklisted {
		if !instanceprice.IsBlacklisted(name) {
			t.Errorf("IsBlacklisted(%q) = false, want true", name)
		}
	}
	notBlacklisted := []string{
		"n2-standard-8",
		"e2-micro",
		"a3-highgpu-8g",
	}
	for _, name := range notBlacklisted {
		if instanceprice.IsBlacklisted(name) {
			t.Errorf("IsBlacklisted(%q) = true, want false", name)
		}
	}
}
