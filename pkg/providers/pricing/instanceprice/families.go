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

package instanceprice

// familyMatcher maps a machine type family prefix to the billing SKU description
// prefixes used to identify its CPU and RAM SKUs.
//
// Matching strategy:
//  1. Strip any leading "Spot Preemptible " from the SKU description.
//  2. Check strings.HasPrefix(normalizedDesc, cpuPrefix) → CPU SKU.
//  3. Check strings.HasPrefix(normalizedDesc, ramPrefix) → RAM SKU.
//  4. Check cpuPremiumPrefix / ramPremiumPrefix for upgrade premium SKUs (m2 only).
//
// Using "Core"/"Ram" in the prefix makes each (family, component) pair unique,
// so no additional Category.ResourceGroup check is required.
//
// Source: https://github.com/Cyclenerd/google-cloud-pricing-cost-calculator/blob/master/build/mapping.csv
// Cross-referenced with: https://github.com/GoogleCloudPlatform/autopilot-cost-calculator/blob/main/calculator/pricing.go
type familyMatcher struct {
	cpuPrefix        string // description prefix (after stripping "Spot Preemptible ") for CPU SKUs
	ramPrefix        string // description prefix (after stripping "Spot Preemptible ") for RAM SKUs
	cpuAltPrefix     string // alternate CPU prefix for regions that use a different SKU format (e.g. c2 in ME regions)
	ramAltPrefix     string // alternate RAM prefix for regions that use a different SKU format
	cpuPremiumPrefix string // non-empty only for families with upgrade-premium CPU SKUs (m2)
	ramPremiumPrefix string // non-empty only for families with upgrade-premium RAM SKUs (m2)
}

type familyMatcherEntry struct {
	key string
	familyMatcher
}

// familyMatchers maps machine type name prefixes (first segment before "-") to
// billing SKU description matchers. Iteration order is deterministic; entries
// that share a base SKU prefix must be consecutive and ordered most-specific
// first (m2 before m1) so that the premium-prefix check in matchPremiumSKU
// always resolves before the base check in matchSKU.
//
// M1 and M2 share the same base billing SKU prefix ("Memory-optimized Instance").
// Because iteration is ordered, the base SKU consistently lands in m2.
// propagateSharedM1M2 then copies those base prices into m1.
//
// Examples of machine type → key:
//
//	"n2-standard-16"    → "n2"
//	"n2d-standard-8"    → "n2d"
//	"c3d-standard-8"    → "c3d"
//	"n2-custom-4-16384" → "n2"
var familyMatchers = []familyMatcherEntry{
	// General purpose — Intel/AMD
	{"e2", familyMatcher{cpuPrefix: "E2 Instance Core", ramPrefix: "E2 Instance Ram"}},
	{"n1", familyMatcher{cpuPrefix: "N1 Predefined Instance Core", ramPrefix: "N1 Predefined Instance Ram"}},
	{"n2", familyMatcher{cpuPrefix: "N2 Instance Core", ramPrefix: "N2 Instance Ram"}},
	{"n2d", familyMatcher{cpuPrefix: "N2D AMD Instance Core", ramPrefix: "N2D AMD Instance Ram"}},
	{"n4", familyMatcher{cpuPrefix: "N4 Instance Core", ramPrefix: "N4 Instance Ram"}},
	{"n4d", familyMatcher{cpuPrefix: "N4D Instance Core", ramPrefix: "N4D Instance Ram"}},

	// General purpose — Arm
	{"n4a", familyMatcher{cpuPrefix: "N4A Instance Core", ramPrefix: "N4A Instance Ram"}},
	{"t2a", familyMatcher{cpuPrefix: "T2A Arm Instance Core", ramPrefix: "T2A Arm Instance Ram"}},

	// Scale-out — AMD
	{"t2d", familyMatcher{cpuPrefix: "T2D AMD Instance Core", ramPrefix: "T2D AMD Instance Ram"}},

	// Compute-optimized
	// Americas uses "Compute optimized Core/Ram"; ME/newer regions keep the old
	// format "Compute optimized Instance Core/Ram". Both map to c2.
	{"c2", familyMatcher{
		cpuPrefix:    "Compute optimized Core",
		ramPrefix:    "Compute optimized Ram",
		cpuAltPrefix: "Compute optimized Instance Core",
		ramAltPrefix: "Compute optimized Instance Ram",
	}},
	{"c2d", familyMatcher{cpuPrefix: "C2D AMD Instance Core", ramPrefix: "C2D AMD Instance Ram"}},
	{"c3", familyMatcher{cpuPrefix: "C3 Instance Core", ramPrefix: "C3 Instance Ram"}},
	{"c3d", familyMatcher{cpuPrefix: "C3D Instance Core", ramPrefix: "C3D Instance Ram"}},
	{"c4", familyMatcher{cpuPrefix: "C4 Instance Core", ramPrefix: "C4 Instance Ram"}},
	{"c4d", familyMatcher{cpuPrefix: "C4D Instance Core", ramPrefix: "C4D Instance Ram"}},

	// Compute-optimized — Arm
	{"c4a", familyMatcher{cpuPrefix: "C4A Arm Instance Core", ramPrefix: "C4A Arm Instance Ram"}},

	// Memory-optimized
	// M2 is listed before M1: both share the same base SKU prefix
	// ("Memory-optimized Instance Core/Ram"), so with deterministic slice
	// iteration the base SKU always lands in m2. propagateSharedM1M2 copies
	// it to m1. M2 additionally has upgrade-premium SKUs.
	{"m2", familyMatcher{
		cpuPrefix:        "Memory-optimized Instance Core",
		ramPrefix:        "Memory-optimized Instance Ram",
		cpuPremiumPrefix: "Memory Optimized Upgrade Premium for Memory-optimized Instance Core",
		ramPremiumPrefix: "Memory Optimized Upgrade Premium for Memory-optimized Instance Ram",
	}},
	{"m1", familyMatcher{cpuPrefix: "Memory-optimized Instance Core", ramPrefix: "Memory-optimized Instance Ram"}},
	{"m3", familyMatcher{cpuPrefix: "M3 Memory-optimized Instance Core", ramPrefix: "M3 Memory-optimized Instance Ram"}},
	{"m4", familyMatcher{cpuPrefix: "M4 Instance Core", ramPrefix: "M4 Instance Ram"}},
	{"m4ultramem224", familyMatcher{cpuPrefix: "M4Ultramem224 Instance Core", ramPrefix: "M4Ultramem224 Instance Ram"}},

	// Accelerator-optimized
	{"a2", familyMatcher{cpuPrefix: "A2 Instance Core", ramPrefix: "A2 Instance Ram"}},
	{"a3", familyMatcher{cpuPrefix: "A3 Instance Core", ramPrefix: "A3 Instance Ram"}},
	{"a3plus", familyMatcher{cpuPrefix: "A3Plus Instance Core", ramPrefix: "A3Plus Instance Ram"}},
	{"a3ultra", familyMatcher{cpuPrefix: "A3Ultra Instance Core", ramPrefix: "A3Ultra Instance Ram"}},
	{"g2", familyMatcher{cpuPrefix: "G2 Instance Core", ramPrefix: "G2 Instance Ram"}},
	{"g4", familyMatcher{cpuPrefix: "G4 Instance Core", ramPrefix: "G4 Instance Ram"}},

	// High-performance computing
	{"h3", familyMatcher{cpuPrefix: "H3 Instance Core", ramPrefix: "H3 Instance Ram"}},
	{"h4d", familyMatcher{cpuPrefix: "H4D Instance Core", ramPrefix: "H4D Instance Ram"}},
	{"z3", familyMatcher{cpuPrefix: "Z3 Instance Core", ramPrefix: "Z3 Instance Ram"}},
}

// prefixEntry pairs a key (accelerator type, machine family, or machine name)
// with a billing SKU description prefix. Used by matchByPrefix which iterates
// in slice order for deterministic matching.
type prefixEntry struct {
	key    string
	prefix string
}

// gpuSKUPrefixes maps a GCE accelerator type string to the billing SKU description prefix
// used to identify its per-GPU price. Both on-demand and spot GPU SKUs share the same
// description prefix; the usage type (UsageType field) distinguishes them.
//
// Accelerator type strings come from MachineType.Accelerators[].GuestAcceleratorType.
var gpuSKUPrefixes = []prefixEntry{
	{"nvidia-tesla-a100", "Nvidia Tesla A100 GPU"},
	{"nvidia-a100-80gb", "Nvidia Tesla A100 80GB GPU"},
	{"nvidia-h100-80gb", "Nvidia H100 80GB GPU"},
	{"nvidia-h100-mega-80gb", "Nvidia H100 80GB Mega GPU"},
	{"nvidia-h100-mega-80gb", "Nvidia H100 80GB Plus GPU"},
	{"nvidia-h200-141gb", "H200 141GB GPU"},
	{"nvidia-l4", "Nvidia L4 GPU"},
	{"nvidia-rtx-pro-6000", "RTX 6000 96GB"},
	{"nvidia-tesla-t4", "Nvidia Tesla T4 GPU"},
	{"nvidia-tesla-v100", "Nvidia Tesla V100 GPU"},
	{"nvidia-tesla-p100", "Nvidia Tesla P100 GPU"},
	{"nvidia-tesla-p4", "Nvidia Tesla P4 GPU"},
	{"nvidia-tesla-k80", "Nvidia Tesla K80 GPU"},
}

// localSSDFamilySKUPrefixes maps a machine family prefix to the billing SKU description
// prefix for its family-specific local SSD price (after stripping "Spot Preemptible ").
// Families not listed here fall back to the generic "SSD backed Local Storage" SKU.
//
// Source: https://github.com/Cyclenerd/google-cloud-pricing-cost-calculator/blob/master/build/mapping.csv
var localSSDFamilySKUPrefixes = []prefixEntry{
	{"c4", "C4 Instance Local SSD"},
	{"c4a", "C4A Instance Local SSD"},
	{"c4d", "C4D Instance Local SSD"},
	{"h4d", "H4D Instance Local SSD"},
}

// flatRateSKUPrefixes maps shared-core machine type names to their billing SKU
// description prefix. These machines are billed at a flat per-instance hourly
// rate rather than the per-vCPU/GiB model used by standard instances.
//
// Only legacy N1 micro/small instances (f1-micro, g1-small) use this model.
// E2 shared-core machines (e2-micro, e2-small, e2-medium) use the standard
// E2 per-vCPU/GiB billing with fractional CPU counts (see sharedCoreCPUFraction).
//
// The "Spot Preemptible " prefix is stripped before matching (same as CPU/RAM SKUs).
var flatRateSKUPrefixes = []prefixEntry{
	{"f1-micro", "Micro Instance with burstable CPU"},
	{"g1-small", "Small Instance with 1 VCPU"},
}

// sharedCoreCPUFraction maps E2 shared-core machine type names to their
// fractional vCPU count used for billing. The Compute API returns IsSharedCpu=true
// and GuestCpus=2 for all three, but billing uses a fraction of that:
//
//   - e2-micro:  0.25 vCPU × E2 CPU rate + 1 GiB × E2 RAM rate
//   - e2-small:  0.50 vCPU × E2 CPU rate + 2 GiB × E2 RAM rate
//   - e2-medium: 1.00 vCPU × E2 CPU rate + 4 GiB × E2 RAM rate
//
// Source: https://cloud.google.com/compute/vm-instance-pricing#sharedcore
var sharedCoreCPUFraction = map[string]float64{
	"e2-micro":  0.25,
	"e2-small":  0.50,
	"e2-medium": 1.00,
}

// machineNameFamilyPrefixOverrides maps machine name prefixes (matched with
// strings.HasPrefix) to the billing family key to use in place of the default
// ExtractFamily result. Needed for machine families whose name prefix does not
// align with their billing SKU family:
//
//   - a3-ultra*: name starts with "a3-" → ExtractFamily returns "a3" (regular A3),
//     but these machines use A3Ultra billing SKUs.
//   - m4-ultramem-224: the 224-vCPU variant has its own M4Ultramem224 billing SKUs
//     with different rates than the standard M4 family.
var machineNameFamilyPrefixOverrides = []struct {
	prefix    string
	familyKey string
}{
	{"a3-ultra", "a3ultra"},
	{"m4-ultramem-224", "m4ultramem224"},
}

// machineTypeExactBlacklist uses exact matching — only the exact name is
// excluded. Covers renamed/legacy machine types that the Compute API may still
// return but that cannot be correctly priced.
var machineTypeExactBlacklist = map[string]bool{
	// Bare z3-highmem names — officially renamed in June 2025:
	//   z3-highmem-88  → z3-highmem-88-highlssd
	//   z3-highmem-176 → z3-highmem-176-standardlssd
	// The Compute API may still return these aliases; blacklist them so we price
	// only the canonical suffixed variants.
	"z3-highmem-88":  true,
	"z3-highmem-176": true,
	// Legacy n1-megamem/ultramem names — rebranded as m1- with different (Memory-optimized)
	// billing SKUs. The Compute API still returns these but they use incorrect N1 Standard
	// rates. Ref: https://cloud.google.com/products/compute/pricing/memory-optimized
	"n1-megamem-96":   true,
	"n1-ultramem-40":  true,
	"n1-ultramem-80":  true,
	"n1-ultramem-160": true,
	// Undocumented M2 PMEM variants — not in official M2 docs which list only
	// m2-ultramem-208/416, m2-megamem-416, m2-hypermem-416.
	// Ref: https://cloud.google.com/compute/docs/memory-optimized-machines#m2_machine_types
	"m2-ultramem2x-96": true,
	"m2-ultramemx-96":  true,
}

// IsBlacklisted reports whether the given machine type name should be excluded
// from pricing computations.
func IsBlacklisted(name string) bool {
	return machineTypeExactBlacklist[name]
}
