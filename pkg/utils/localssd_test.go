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

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLocalSSDTotalGiB(t *testing.T) {
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

		// Machine-level total overrides (wrong PartitionCount from Compute API)
		{"c4d-highmem-8-lssd override", "c4d-highmem-8-lssd", 1, 2250},
		{"c4d-highmem-16-lssd override", "c4d-highmem-16-lssd", 1, 3000},
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
			got := LocalSSDTotalGiB(tt.machineName, tt.partitionCount)
			assert.Equal(t, tt.want, got)
		})
	}
}
