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

package utils

import "strings"

// DefaultSSDPartitionGiB is the standard NVMe local SSD partition size for most GCP machine families.
const DefaultSSDPartitionGiB int64 = 375

// localSSDEntry describes local SSD capacity for a machine family or a specific machine type.
//
// Family entries (key has no "-"): set perPartGiB when the family's partition size differs from
// DefaultSSDPartitionGiB. Matched against the first "-"-delimited segment of the machine name.
//
// Machine entries (key contains "-"): set totalGiB to override the computed value for machines
// where the Compute API returns a wrong PartitionCount.
type localSSDEntry struct {
	totalGiB   int64 // total SSD capacity; 0 = compute from partitions
	perPartGiB int64 // per-partition GiB; 0 = use DefaultSSDPartitionGiB
}

// localSSDTable is the single source of truth for local SSD sizing.
//
// Family-level entries (no "-" in key) override the per-partition GiB for the whole family.
// Machine-level entries (contain "-") override the total GiB for machines whose Compute API
// PartitionCount is wrong.
//
// Source: https://github.com/Cyclenerd/google-cloud-pricing-cost-calculator/blob/master/build/gcp.yml
// Cross-referenced with: https://cloud.google.com/compute/docs/disks/local-ssd
var localSSDTable = map[string]localSSDEntry{
	// z3 uses 3 TiB NVMe per partition; all other families use 375 GiB
	"z3": {perPartGiB: 3000},

	// c4d lssd variants: Compute API reports PartitionCount=1 but actual capacity differs
	"c4d-highmem-8-lssd":  {totalGiB: 2250},  // 6 × 375 GiB
	"c4d-highmem-16-lssd": {totalGiB: 3000},  // 8 × 375 GiB

	// Bare-metal variants use 3000 GiB per partition (not 375 GiB)
	"c4-highmem-288-lssd-metal":     {totalGiB: 18000}, // 6 × 3000 GiB
	"c4-standard-288-lssd-metal":    {totalGiB: 18000}, // 6 × 3000 GiB
	"z3-highmem-192-highlssd-metal": {totalGiB: 72000}, // 12 × 6000 GiB
}

// LocalSSDTotalGiB returns the total local SSD capacity in GiB for a machine.
// It applies machine-specific total overrides first (for machines with wrong API PartitionCount),
// then falls back to partitionCount × per-family partition size.
// Returns 0 when partitionCount is 0 and no machine-level override exists.
func LocalSSDTotalGiB(machineName string, partitionCount int) int64 {
	// Machine-level total override (wrong PartitionCount from Compute API)
	if e, ok := localSSDTable[machineName]; ok && e.totalGiB > 0 {
		return e.totalGiB
	}
	if partitionCount <= 0 {
		return 0
	}
	// Family-level per-partition size
	family := machineName
	if i := strings.IndexByte(machineName, '-'); i > 0 {
		family = machineName[:i]
	}
	giBPerPart := DefaultSSDPartitionGiB
	if e, ok := localSSDTable[family]; ok && e.perPartGiB > 0 {
		giBPerPart = e.perPartGiB
	}
	return int64(partitionCount) * giBPerPart
}
