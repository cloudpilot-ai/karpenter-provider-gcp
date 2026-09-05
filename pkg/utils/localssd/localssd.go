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

package localssd

import (
	"slices"
	"strings"
)

// DefaultPartitionGiB is the standard NVMe local SSD partition size for most GCP machine families.
const DefaultPartitionGiB int64 = 375

// FamilySupportsConfigurableLocalSSDs reports whether a machine family accepts an explicit SSD count.
func FamilySupportsConfigurableLocalSSDs(machineName string) bool {
	_, ok := configurableLocalSSDFamilies[familyPrefix(machineName)]
	return ok
}

// AllowedLocalSSDCounts returns the documented non-zero SSD counts for a machine shape.
// Sources:
//   - https://cloud.google.com/compute/docs/general-purpose-machines
//   - https://cloud.google.com/compute/docs/compute-optimized-machines
func AllowedLocalSSDCounts(machineName string, vCPUs int32) []int {
	family, ok := configurableLocalSSDFamilies[familyPrefix(machineName)]
	if !ok {
		return nil
	}
	if family.fixed != nil {
		return slices.Clone(family.fixed)
	}
	for _, b := range family.brackets {
		if vCPUs >= b.minVCPUs {
			return slices.Clone(b.counts)
		}
	}
	return nil
}

type vcpuBracket struct {
	minVCPUs int32
	counts   []int
}

type configurableFamily struct {
	fixed    []int
	brackets []vcpuBracket
}

var configurableLocalSSDFamilies = map[string]configurableFamily{
	"n1": {fixed: []int{1, 2, 3, 4, 5, 6, 7, 8, 16, 24}},
	"n2": {brackets: []vcpuBracket{
		{minVCPUs: 82, counts: []int{16, 24}},
		{minVCPUs: 42, counts: []int{8, 16, 24}},
		{minVCPUs: 22, counts: []int{4, 8, 16, 24}},
		{minVCPUs: 12, counts: []int{2, 4, 8, 16, 24}},
		{minVCPUs: 2, counts: []int{1, 2, 4, 8, 16, 24}},
	}},
	"n2d": {brackets: []vcpuBracket{
		{minVCPUs: 96, counts: []int{8, 16, 24}},
		{minVCPUs: 64, counts: []int{4, 8, 16, 24}},
		{minVCPUs: 32, counts: []int{2, 4, 8, 16, 24}},
		{minVCPUs: 2, counts: []int{1, 2, 4, 8, 16, 24}},
	}},
	"c2": {brackets: []vcpuBracket{
		{minVCPUs: 60, counts: []int{8}},
		{minVCPUs: 30, counts: []int{4, 8}},
		{minVCPUs: 16, counts: []int{2, 4, 8}},
		{minVCPUs: 4, counts: []int{1, 2, 4, 8}},
	}},
	"c2d": {brackets: []vcpuBracket{
		{minVCPUs: 112, counts: []int{8}},
		{minVCPUs: 56, counts: []int{4, 8}},
		{minVCPUs: 32, counts: []int{2, 4, 8}},
		{minVCPUs: 2, counts: []int{1, 2, 4, 8}},
	}},
}

func familyPrefix(machineName string) string {
	if i := strings.IndexByte(machineName, '-'); i > 0 {
		return machineName[:i]
	}
	return machineName
}

type entry struct {
	totalGiB   int64 // total SSD capacity; 0 = compute from partitions
	perPartGiB int64 // per-partition GiB; 0 = use DefaultPartitionGiB
}

// table maps machine families (no "-") and specific machine types (contains "-") to their
// local SSD sizing. Family entries override per-partition GiB; machine entries override the total
// for machines where the Compute API returns a wrong PartitionCount.
//
// Source: https://github.com/Cyclenerd/google-cloud-pricing-cost-calculator/blob/master/build/gcp.yml
// Cross-referenced with: https://cloud.google.com/compute/docs/disks/local-ssd
var table = map[string]entry{
	// z3 uses 3 TiB NVMe per partition; all other families use 375 GiB
	"z3": {perPartGiB: 3000},

	// Bare-metal variants use 3000 GiB per partition (not 375 GiB)
	"c4-highmem-288-lssd-metal":     {totalGiB: 18000}, // 6 × 3000 GiB
	"c4-standard-288-lssd-metal":    {totalGiB: 18000}, // 6 × 3000 GiB
	"z3-highmem-192-highlssd-metal": {totalGiB: 72000}, // 12 × 6000 GiB
}

// TotalGiB returns total local SSD capacity in GiB for the given machine type.
// Machine-level total overrides take priority (for machines where the API reports a wrong
// PartitionCount); otherwise falls back to partitionCount × per-family partition size.
func TotalGiB(machineName string, partitionCount int) int64 {
	if e, ok := table[machineName]; ok && e.totalGiB > 0 {
		return e.totalGiB
	}
	if partitionCount <= 0 {
		return 0
	}
	return int64(partitionCount) * partitionSizeGiB(machineName)
}

// partitionSizeGiB returns the GiB capacity of a single local SSD partition for the given
// machine type, using a family-level override from table or DefaultPartitionGiB.
func partitionSizeGiB(machineName string) int64 {
	family := machineName
	if i := strings.IndexByte(machineName, '-'); i > 0 {
		family = machineName[:i]
	}
	if e, ok := table[family]; ok && e.perPartGiB > 0 {
		return e.perPartGiB
	}
	return DefaultPartitionGiB
}
