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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTotalGiB(t *testing.T) {
	tests := []struct {
		name           string
		machineName    string
		partitionCount int
		want           int64
	}{
		// Standard family: partitionCount × 375 GiB
		{"n2 standard: 2 × 375", "n2-standard-8", 2, 750},
		{"c3d lssd: 2 × 375", "c3d-highmem-8-lssd", 2, 750},

		// z3 family: partitionCount × 3000 GiB
		{"z3 standard: 4 × 3000", "z3-highmem-88-standardlssd", 4, 12000},
		{"z3 high: 2 × 3000", "z3-highmem-176-highlssd", 2, 6000},

		{"c4d-highmem-8-lssd: 1 × 375", "c4d-highmem-8-lssd", 1, 375},
		{"c4d-highmem-16-lssd: 1 × 375", "c4d-highmem-16-lssd", 1, 375},

		// Machine-level total overrides (wrong PartitionCount from Compute API)
		{"c4-highmem-288-lssd-metal override", "c4-highmem-288-lssd-metal", 6, 18000},
		{"c4-standard-288-lssd-metal override", "c4-standard-288-lssd-metal", 6, 18000},
		{"z3-highmem-192-highlssd-metal override", "z3-highmem-192-highlssd-metal", 12, 72000},

		// Edge cases
		{"zero partitions", "n2-standard-8", 0, 0},
		{"negative partitions", "n2-standard-8", -1, 0},
		{"empty name, zero partitions", "", 0, 0},
		{"empty name, nonzero partitions uses default", "", 1, 375},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TotalGiB(tt.machineName, tt.partitionCount)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFamilySupportsConfigurableLocalSSDs(t *testing.T) {
	tests := []struct {
		machineName string
		want        bool
	}{
		{"n1-standard-8", true},
		{"n2-standard-8", true},
		{"n2d-standard-8", true},
		{"c2-standard-8", true},
		{"c2d-standard-8", true},

		{"c4d-standard-8-lssd", false},
		{"c4-standard-8-lssd", false},
		{"c4a-standard-4-lssd", false},
		{"z3-highmem-22-standardlssd", false},
		{"a3-highgpu-8g", false},

		{"e2-standard-2", false},
		{"t2a-standard-1", false},

		{"n2asomething", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.machineName, func(t *testing.T) {
			got := FamilySupportsConfigurableLocalSSDs(tt.machineName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAllowedLocalSSDCounts(t *testing.T) {
	tests := []struct {
		name        string
		machineName string
		vCPUs       int32
		want        []int
	}{
		{"n1-standard-1", "n1-standard-1", 1, []int{1, 2, 3, 4, 5, 6, 7, 8, 16, 24}},
		{"n1-standard-8", "n1-standard-8", 8, []int{1, 2, 3, 4, 5, 6, 7, 8, 16, 24}},
		{"n1-highmem-96", "n1-highmem-96", 96, []int{1, 2, 3, 4, 5, 6, 7, 8, 16, 24}},

		{"n2-standard-2 (lowest bracket)", "n2-standard-2", 2, []int{1, 2, 4, 8, 16, 24}},
		{"n2-standard-8", "n2-standard-8", 8, []int{1, 2, 4, 8, 16, 24}},
		{"n2-standard-16 (min jumps to 2)", "n2-standard-16", 16, []int{2, 4, 8, 16, 24}},
		{"n2-standard-32", "n2-standard-32", 32, []int{4, 8, 16, 24}},
		{"n2-standard-48", "n2-standard-48", 48, []int{8, 16, 24}},
		{"n2-standard-80", "n2-standard-80", 80, []int{8, 16, 24}},
		{"n2-standard-96 (top bracket)", "n2-standard-96", 96, []int{16, 24}},
		{"n2-standard-128", "n2-standard-128", 128, []int{16, 24}},

		{"n2 hypothetical 10-vCPU (top of lowest bracket)", "n2-foo-10", 10, []int{1, 2, 4, 8, 16, 24}},
		{"n2 hypothetical 22-vCPU (start of 22-40 bracket)", "n2-foo-22", 22, []int{4, 8, 16, 24}},
		{"n2 hypothetical 42-vCPU (start of 42-80 bracket)", "n2-foo-42", 42, []int{8, 16, 24}},
		{"n2 hypothetical 82-vCPU (start of top bracket)", "n2-foo-82", 82, []int{16, 24}},

		{"n2d-standard-2", "n2d-standard-2", 2, []int{1, 2, 4, 8, 16, 24}},
		{"n2d-standard-16 (still min=1)", "n2d-standard-16", 16, []int{1, 2, 4, 8, 16, 24}},
		{"n2d-standard-32", "n2d-standard-32", 32, []int{2, 4, 8, 16, 24}},
		{"n2d-standard-48", "n2d-standard-48", 48, []int{2, 4, 8, 16, 24}},
		{"n2d-standard-64", "n2d-standard-64", 64, []int{4, 8, 16, 24}},
		{"n2d-standard-80", "n2d-standard-80", 80, []int{4, 8, 16, 24}},
		{"n2d-standard-96", "n2d-standard-96", 96, []int{8, 16, 24}},
		{"n2d-standard-224", "n2d-standard-224", 224, []int{8, 16, 24}},

		{"c2-standard-4 (lowest)", "c2-standard-4", 4, []int{1, 2, 4, 8}},
		{"c2-standard-8", "c2-standard-8", 8, []int{1, 2, 4, 8}},
		{"c2-standard-16", "c2-standard-16", 16, []int{2, 4, 8}},
		{"c2-standard-30", "c2-standard-30", 30, []int{4, 8}},
		{"c2-standard-60", "c2-standard-60", 60, []int{8}},

		{"c2d-standard-2", "c2d-standard-2", 2, []int{1, 2, 4, 8}},
		{"c2d-standard-16", "c2d-standard-16", 16, []int{1, 2, 4, 8}},
		{"c2d-standard-32", "c2d-standard-32", 32, []int{2, 4, 8}},
		{"c2d-standard-56", "c2d-standard-56", 56, []int{4, 8}},
		{"c2d-standard-112", "c2d-standard-112", 112, []int{8}},

		{"n2-highcpu-32 same as n2-standard-32", "n2-highcpu-32", 32, []int{4, 8, 16, 24}},
		{"n2d-highmem-16 same as n2d-standard-16", "n2d-highmem-16", 16, []int{1, 2, 4, 8, 16, 24}},
		{"c2d-highmem-56 same as c2d-standard-56", "c2d-highmem-56", 56, []int{4, 8}},

		{"non-configurable family (e2)", "e2-standard-2", 2, nil},
		{"bundled SKU (c4d-lssd)", "c4d-highmem-8-lssd", 8, nil},
		{"zero vCPUs on configurable", "n2-standard-8", 0, nil},
		{"empty name", "", 8, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AllowedLocalSSDCounts(tt.machineName, tt.vCPUs)
			assert.Equal(t, tt.want, got)
		})
	}
}
