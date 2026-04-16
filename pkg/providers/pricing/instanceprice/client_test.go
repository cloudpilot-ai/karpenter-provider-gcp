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
	"strings"
	"testing"

	"google.golang.org/api/cloudbilling/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/pricing/instanceprice"
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

func TestExtractFamily(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"n2-standard-16", "n2"},
		{"n2d-standard-8", "n2d"},
		{"c3d-standard-8", "c3d"},
		{"n2-custom-4-16384", "n2"},
		{"e2-micro", "e2"},
		{"m1-megamem-96", "m1"},
		{"a3-highgpu-8g", "a3"},
		{"t2a-standard-1", "t2a"},
		{"barebone", "barebone"}, // no dash — returns whole string
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := instanceprice.ExtractFamily(tt.input); got != tt.want {
				t.Errorf("ExtractFamily(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
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
			sku:       makeSKULocal("N2 Instance Core", "h", 0, 31611000), // $0.031611/h
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
			sku:       makeSKULocal("N2 Instance Ram", "GBy.h", 0, 4237000),
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

func TestMatchPremiumSKU(t *testing.T) {
	tests := []struct {
		desc          string
		wantFamily    string
		wantComponent string
		wantOK        bool
	}{
		{
			"Memory Optimized Upgrade Premium for Memory-optimized Instance Core running in Americas",
			"m2", "CPU", true,
		},
		{
			"Memory Optimized Upgrade Premium for Memory-optimized Instance Ram running in Americas",
			"m2", "RAM", true,
		},
		{
			"Spot Preemptible Memory Optimized Upgrade Premium for Memory-optimized Instance Core running in Americas",
			"m2", "CPU", true,
		},
		// Regular SKU — not a premium
		{"Memory-optimized Instance Core running in Americas", "", "", false},
		{"N2 Instance Core running in Americas", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			fam, comp, ok := instanceprice.MatchPremiumSKU(tt.desc)
			if ok != tt.wantOK {
				t.Fatalf("MatchPremiumSKU(%q) ok=%v, want %v", tt.desc, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if fam != tt.wantFamily {
				t.Errorf("MatchPremiumSKU(%q) family=%q, want %q", tt.desc, fam, tt.wantFamily)
			}
			if comp != tt.wantComponent {
				t.Errorf("MatchPremiumSKU(%q) component=%q, want %q", tt.desc, comp, tt.wantComponent)
			}
		})
	}
}

func TestMatchFlatRateSKUs(t *testing.T) {
	const spotPreemptiblePrefix = "Spot Preemptible "
	tests := []struct {
		desc            string
		wantMachineType string
		wantOK          bool
	}{
		// f1-micro and g1-small use flat-rate per-instance billing
		{"Micro Instance with burstable CPU running in Americas", "f1-micro", true},
		{"Spot Preemptible Micro Instance with burstable CPU running in Americas", "f1-micro", true},
		{"Small Instance with 1 VCPU running in Americas", "g1-small", true},
		{"Spot Preemptible Small Instance with 1 VCPU running in Americas", "g1-small", true},
		// E2 shared-core uses fractional-vCPU model, not flat-rate → should not match
		{"E2 Micro Instance running in Americas", "", false},
		{"E2 Small Instance running in Americas", "", false},
		{"E2 Medium Instance running in Americas", "", false},
		// Standard instance SKUs should not match
		{"E2 Instance Core running in Americas", "", false},
		{"N2 Instance Core running in Americas", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			desc := strings.TrimPrefix(tt.desc, spotPreemptiblePrefix)
			mt, ok := instanceprice.MatchByFlatRateSKUPrefix(desc)
			if ok != tt.wantOK {
				t.Fatalf("MatchByFlatRateSKUPrefix(%q) ok=%v, want %v", tt.desc, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if mt != tt.wantMachineType {
				t.Errorf("MatchByFlatRateSKUPrefix(%q) machineType=%q, want %q", tt.desc, mt, tt.wantMachineType)
			}
		})
	}
}

func TestProcessSKUs(t *testing.T) {
	const region = "us-central1"

	baseSKUs := func() []*cloudbilling.Sku {
		return []*cloudbilling.Sku{
			// N2 on-demand CPU and RAM
			makeSKU("N2 Instance Core running in Americas", "OnDemand", 31611000),
			makeSKU("N2 Instance Ram running in Americas", "OnDemand", 4237000),
			// N2 spot CPU and RAM
			makeSKU("Spot Preemptible N2 Instance Core running in Americas", "Preemptible", 9500000),
			makeSKU("Spot Preemptible N2 Instance Ram running in Americas", "Preemptible", 1000000),
			// M1 base prices (shared with m2 via propagateSharedM1M2)
			makeSKU("Memory-optimized Instance Core running in Americas", "OnDemand", 100000000),
			makeSKU("Memory-optimized Instance Ram running in Americas", "OnDemand", 10000000),
			// M2 upgrade premium
			makeSKU("Memory Optimized Upgrade Premium for Memory-optimized Instance Core running in Americas", "OnDemand", 50000000),
			// SKU for a different region — must be ignored
			makeSKU("N2 Instance Core running in Europe", "OnDemand", 99999999, "europe-west1"),
			// SKU with nil Category — must be ignored
			{Description: "Bad SKU", ServiceRegions: []string{region}},
		}
	}
	result := func() *instanceprice.RegionUnitPrices {
		return instanceprice.ProcessSKUs(baseSKUs(), region)
	}

	t.Run("n2_on_demand", func(t *testing.T) {
		requireN2OnDemand(t, result())
	})

	t.Run("n2_spot", func(t *testing.T) {
		requireN2Spot(t, result())
	})

	t.Run("wrong_region_excluded", func(t *testing.T) {
		requireWrongRegionExcluded(t, result())
	})

	t.Run("m1_m2_propagation", func(t *testing.T) {
		requireSharedBaseRates(t, result())
	})

	t.Run("m2_premium", func(t *testing.T) {
		requireM2PremiumOnly(t, result())
	})
}

func requireN2OnDemand(t *testing.T, prices *instanceprice.RegionUnitPrices) {
	t.Helper()
	n2 := prices.Families["n2"]
	if n2.CPUOnDemandPerHour == 0 {
		t.Error("n2 CPUOnDemandPerHour should be non-zero")
	}
	if n2.RAMOnDemandPerGiB == 0 {
		t.Error("n2 RAMOnDemandPerGiB should be non-zero")
	}
}

func requireN2Spot(t *testing.T, prices *instanceprice.RegionUnitPrices) {
	t.Helper()
	n2 := prices.Families["n2"]
	if n2.CPUSpotPerHour == 0 {
		t.Error("n2 CPUSpotPerHour should be non-zero")
	}
	if n2.RAMSpotPerGiB == 0 {
		t.Error("n2 RAMSpotPerGiB should be non-zero")
	}
}

func requireWrongRegionExcluded(t *testing.T, prices *instanceprice.RegionUnitPrices) {
	t.Helper()
	n2 := prices.Families["n2"]
	if n2.CPUOnDemandPerHour >= 0.099 {
		t.Errorf("n2 CPUOnDemandPerHour looks contaminated by wrong-region SKU: %v", n2.CPUOnDemandPerHour)
	}
}

func requireSharedBaseRates(t *testing.T, prices *instanceprice.RegionUnitPrices) {
	t.Helper()
	m1 := prices.Families["m1"]
	m2 := prices.Families["m2"]
	if m1.CPUOnDemandPerHour == 0 {
		t.Error("m1 CPUOnDemandPerHour should be propagated from shared base SKU")
	}
	if m2.CPUOnDemandPerHour == 0 {
		t.Error("m2 CPUOnDemandPerHour should be propagated from shared base SKU")
	}
	if m1.CPUOnDemandPerHour != m2.CPUOnDemandPerHour {
		t.Errorf("m1 and m2 base CPU price should be equal: m1=%v m2=%v", m1.CPUOnDemandPerHour, m2.CPUOnDemandPerHour)
	}
}

func requireM2PremiumOnly(t *testing.T, prices *instanceprice.RegionUnitPrices) {
	t.Helper()
	m1 := prices.Families["m1"]
	m2 := prices.Families["m2"]
	if m2.CPUPremiumOnDemandPerHour == 0 {
		t.Error("m2 CPUPremiumOnDemandPerHour should be set from upgrade premium SKU")
	}
	if m1.CPUPremiumOnDemandPerHour != 0 {
		t.Error("m1 CPUPremiumOnDemandPerHour should remain zero")
	}
}

func TestProcessSKUs_PreferHigherSpotRate(t *testing.T) {
	const region = "us-central1"

	skus := []*cloudbilling.Sku{
		makeSKU("N2 Instance Core running in Americas", "OnDemand", 31611000),
		makeSKU("N2 Instance Ram running in Americas", "OnDemand", 4237000),
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

func TestParseLocalSSDCount(t *testing.T) {
	tests := []struct {
		desc string
		want int
	}{
		{"4 vCPUs, 16 GB RAM, 1 local SSD", 1},
		{"16 vCPUs, 60 GB RAM, 2 local SSDs", 2},
		{"14 vCPUs, 112 GB RAM, 1 Local SSDs", 1},
		{"16 vCPUs, 128 GB RAM, 2 Local SSDs", 2},
		{"4 vCPUs, 16 GB RAM", 0}, // no SSD
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := instanceprice.ParseLocalSSDCount(tt.desc); got != tt.want {
				t.Errorf("ParseLocalSSDCount(%q) = %d, want %d", tt.desc, got, tt.want)
			}
		})
	}
}

func TestResolveFamily(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
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

func TestAssemblePrices(t *testing.T) {
	const region = "us-central1"

	approxEqual := func(a, b float64) bool {
		return math.Abs(a-b) < 1e-9
	}

	assertSingle := func(t *testing.T, prices []instanceprice.MachinePrice, wantOD, wantSpot float64) {
		t.Helper()
		if len(prices) != 1 {
			t.Fatalf("got %d prices, want 1", len(prices))
		}
		p := prices[0]
		if !approxEqual(p.OnDemandPerHour, wantOD) {
			t.Errorf("OnDemandPerHour = %v, want %v", p.OnDemandPerHour, wantOD)
		}
		if !approxEqual(p.SpotPerHour, wantSpot) {
			t.Errorf("SpotPerHour = %v, want %v", p.SpotPerHour, wantSpot)
		}
	}

	t.Run("standard on-demand only", func(t *testing.T) {
		// n2-standard-2: 2 vCPUs, 8 GiB. OD = 2*0.50 + 8.0*0.10 = 1.80
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"n2": {CPUOnDemandPerHour: 0.50, RAMOnDemandPerGiB: 0.10},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"n2-standard-2": {VCPUs: 2, MemoryMiB: 8192},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 1.80, 0)
	})

	t.Run("standard with spot", func(t *testing.T) {
		// OD = 1.80, Spot = 2*0.15 + 8.0*0.03 = 0.54
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"n2": {CPUOnDemandPerHour: 0.50, RAMOnDemandPerGiB: 0.10, CPUSpotPerHour: 0.15, RAMSpotPerGiB: 0.03},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"n2-standard-2": {VCPUs: 2, MemoryMiB: 8192},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 1.80, 0.54)
	})

	t.Run("shared-core e2-micro on-demand", func(t *testing.T) {
		// e2-micro: frac=0.25, 1 GiB. OD = 0.25*1.00 + 1.0*0.20 = 0.45
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"e2": {CPUOnDemandPerHour: 1.00, RAMOnDemandPerGiB: 0.20},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"e2-micro": {VCPUs: 2, MemoryMiB: 1024, IsSharedCPU: true},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 0.45, 0)
	})

	t.Run("shared-core e2-micro with spot", func(t *testing.T) {
		// Spot = 0.25*0.30 + 1.0*0.06 = 0.135
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"e2": {CPUOnDemandPerHour: 1.00, RAMOnDemandPerGiB: 0.20, CPUSpotPerHour: 0.30, RAMSpotPerGiB: 0.06},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"e2-micro": {VCPUs: 2, MemoryMiB: 1024, IsSharedCPU: true},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 0.45, 0.135)
	})

	t.Run("flat-rate f1-micro", func(t *testing.T) {
		// f1-micro is not in sharedCoreCPUFraction → falls through to FlatRatePrices
		rp := &instanceprice.RegionUnitPrices{
			FlatRatePrices: map[string]instanceprice.PerUnitPrice{
				"f1-micro": {OnDemandPerHour: 0.0076, SpotPerHour: 0.0021},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"f1-micro": {VCPUs: 1, MemoryMiB: 614, IsSharedCPU: true},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 0.0076, 0.0021)
	})

	t.Run("gpu machine", func(t *testing.T) {
		// a2-highgpu-1g: 12 vCPUs, 85 GiB, 1x A100
		// OD = 12*0.03 + 85.0*0.004 + 1*2.93 = 0.36+0.34+2.93 = 3.63
		// Spot = 12*0.009 + 85.0*0.0012 + 1*0.88 = 0.108+0.102+0.88 = 1.09
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"a2": {CPUOnDemandPerHour: 0.03, RAMOnDemandPerGiB: 0.004, CPUSpotPerHour: 0.009, RAMSpotPerGiB: 0.0012},
			},
			GPUs: map[string]instanceprice.PerUnitPrice{
				"nvidia-tesla-a100": {OnDemandPerHour: 2.93, SpotPerHour: 0.88},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"a2-highgpu-1g": {VCPUs: 12, MemoryMiB: 87040, Accelerators: []instanceprice.AcceleratorSpec{{Type: "nvidia-tesla-a100", Count: 1}}},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 3.63, 1.09)
	})

	t.Run("local ssd family-specific rate", func(t *testing.T) {
		// c4-standard-8: 8 vCPUs, 32 GiB, 375 GiB SSD (family-specific rate)
		// OD = 8*0.04 + 32.0*0.005 + 375*0.0001 = 0.32+0.16+0.0375 = 0.5175
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"c4": {CPUOnDemandPerHour: 0.04, RAMOnDemandPerGiB: 0.005},
			},
			FamilyLocalSSD: map[string]instanceprice.LocalSSDRate{
				"c4": {OnDemandPerGiBHour: 0.0001},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"c4-standard-8": {VCPUs: 8, MemoryMiB: 32768, LocalSSDGiB: 375},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 0.5175, 0)
	})

	t.Run("local ssd generic rate", func(t *testing.T) {
		// n2-standard-8: 8 vCPUs, 32 GiB, 375 GiB SSD (no family SSD entry → generic)
		// OD = 8*0.50 + 32.0*0.10 + 375*0.00012 = 4.0+3.2+0.045 = 7.245
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"n2": {CPUOnDemandPerHour: 0.50, RAMOnDemandPerGiB: 0.10},
			},
			SSDOnDemandPerGiBHour: 0.00012,
		}
		specs := map[string]instanceprice.MachineSpec{
			"n2-standard-8": {VCPUs: 8, MemoryMiB: 32768, LocalSSDGiB: 375},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 7.245, 0)
	})

	t.Run("m2 premium pricing", func(t *testing.T) {
		// m2-ultramem-208: 208 vCPUs, 5632 GiB
		// OD = 208*(0.04+0.01) + 5632*(0.006+0.002) = 10.4+45.056 = 55.456
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"m2": {
					CPUOnDemandPerHour:        0.04,
					RAMOnDemandPerGiB:         0.006,
					CPUPremiumOnDemandPerHour: 0.01,
					RAMPremiumOnDemandPerGiB:  0.002,
				},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"m2-ultramem-208": {VCPUs: 208, MemoryMiB: 5767168}, // 5632 GiB
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 55.456, 0)
	})

	t.Run("missing family skipped", func(t *testing.T) {
		// unknown-type-4 has no matching family → AssemblePrices returns empty slice
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"n2": {CPUOnDemandPerHour: 0.50, RAMOnDemandPerGiB: 0.10},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"unknown-type-4": {VCPUs: 4, MemoryMiB: 16384},
		}
		prices := instanceprice.AssemblePrices(region, rp, specs)
		if len(prices) != 0 {
			t.Errorf("got %d prices, want 0 (missing family should be skipped)", len(prices))
		}
	})

	t.Run("ssd total override", func(t *testing.T) {
		// c4d-highmem-8-lssd: override applied at spec-build time → spec.LocalSSDGiB = 2250 (6 × 375)
		// OD = 8*0.04 + 64.0*0.005 + 2250*0.0001 = 0.32+0.32+0.225 = 0.865
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"c4d": {CPUOnDemandPerHour: 0.04, RAMOnDemandPerGiB: 0.005},
			},
			FamilyLocalSSD: map[string]instanceprice.LocalSSDRate{
				"c4d": {OnDemandPerGiBHour: 0.0001},
			},
		}
		specs := map[string]instanceprice.MachineSpec{
			"c4d-highmem-8-lssd": {VCPUs: 8, MemoryMiB: 65536, LocalSSDGiB: 2250},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 0.865, 0)
	})

	t.Run("lssd no family SSD SKU means bundled free", func(t *testing.T) {
		// c4-standard-8-lssd in a region without a "C4 Instance Local SSD" SKU:
		// no family-specific rate → SSD is bundled at $0, generic rate NOT used.
		// OD = 8*0.04 + 30.0*0.005 = 0.32+0.15 = 0.47
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"c4": {CPUOnDemandPerHour: 0.04, RAMOnDemandPerGiB: 0.005},
			},
			SSDOnDemandPerGiBHour: 0.0001, // generic rate — must NOT be used for -lssd
		}
		specs := map[string]instanceprice.MachineSpec{
			"c4-standard-8-lssd": {VCPUs: 8, MemoryMiB: 30720, LocalSSDGiB: 375},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 0.47, 0)
	})

	t.Run("non-lssd falls back to generic SSD rate", func(t *testing.T) {
		// c4-standard-8 (no -lssd suffix): no family-specific rate → uses generic.
		// OD = 8*0.04 + 30.0*0.005 + 375*0.0001 = 0.32+0.15+0.0375 = 0.5075
		rp := &instanceprice.RegionUnitPrices{
			Families: map[string]instanceprice.FamilyUnitPrices{
				"c4": {CPUOnDemandPerHour: 0.04, RAMOnDemandPerGiB: 0.005},
			},
			SSDOnDemandPerGiBHour: 0.0001,
		}
		specs := map[string]instanceprice.MachineSpec{
			"c4-standard-8": {VCPUs: 8, MemoryMiB: 30720, LocalSSDGiB: 375},
		}
		assertSingle(t, instanceprice.AssemblePrices(region, rp, specs), 0.5075, 0)
	})
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
