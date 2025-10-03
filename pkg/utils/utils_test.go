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

func TestResolveReservedCPUMCore(t *testing.T) {
	testCases := []struct {
		instanceType string
		cpuMCore     int64
		expected     int64
	}{
		// Shared-core E2 types
		{"e2-micro", 1000, 1060},
		{"e2-small", 2000, 1060},
		{"e2-medium", 2000, 1060},

		// Standard types
		{"n2-standard-1", 1000, 60},
		{"n2-standard-2", 2000, 70},
		{"n2-standard-4", 4000, 80},
		{"n2-standard-8", 8000, 90},
		{"n2-standard-16", 16000, 110},
	}

	for _, tt := range testCases {
		t.Run(tt.instanceType+"-"+string(rune(tt.cpuMCore)), func(t *testing.T) {
			cpu := ResolveReservedCPUMCore(tt.instanceType, tt.cpuMCore)
			assert.Equal(t, tt.expected, cpu)
		})
	}
}

func TestResolveReservedMemoryMiB(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		memoryMiB    int64
		expected     int64
	}{
		{
			name:         "<=1024MiB",
			instanceType: "n2-standard-2",
			memoryMiB:    512,
			expected:     255,
		},
		{
			name:         "exactly 1024MiB",
			instanceType: "n2-standard-2",
			memoryMiB:    1024,
			expected:     255,
		},
		{
			name:         "just above 1024MiB",
			instanceType: "n2-standard-2",
			memoryMiB:    1025,
			expected:     256,
		},
		{
			name:         "4096MiB (first tier max)",
			instanceType: "n2-standard-2",
			memoryMiB:    4096,
			expected:     1024,
		},
		{
			name:         "4097MiB (second tier starts)",
			instanceType: "n2-standard-2",
			memoryMiB:    4097,
			expected:     1024,
		},
		{
			name:         "8192MiB (second tier max)",
			instanceType: "n2-standard-2",
			memoryMiB:    8192,
			expected:     1843,
		},
		{
			name:         "16384MiB (third tier max)",
			instanceType: "n2-standard-2",
			memoryMiB:    16384,
			expected:     2662,
		},
		{
			name:         "131072MiB (fourth tier max)",
			instanceType: "n2-standard-2",
			memoryMiB:    131072,
			expected:     9543,
		},
		{
			name:         ">131072MiB (final tier)",
			instanceType: "n2-standard-2",
			memoryMiB:    150000,
			expected:     9921,
		},
		{
			name:         "very large value",
			instanceType: "n2-standard-2",
			memoryMiB:    200000,
			expected:     10921,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory := ResolveReservedMemoryMiB(tt.instanceType, tt.memoryMiB)
			assert.Equal(t, tt.expected, memory)
		})
	}
}

func TestResolveReservedEphemeralStorage(t *testing.T) {
	tests := []struct {
		name             string
		bootDiskGiB      int64
		totalSSDGiB      int64
		localSSDCount    int64
		expectedEviction int64
		expectedSystem   int64
	}{
		// Boot disk scenarios
		{
			name:             "100GB boot disk only",
			bootDiskGiB:      100,
			totalSSDGiB:      0,
			localSSDCount:    0,
			expectedEviction: 10,  // 10% of 100GB
			expectedSystem:   41,  // min(50, 35+6, 100) = min(50, 41, 100) = 41
		},
		{
			name:             "200GB boot disk only",
			bootDiskGiB:      200,
			totalSSDGiB:      0,
			localSSDCount:    0,
			expectedEviction: 20,  // 10% of 200GB
			expectedSystem:   76,  // min(100, 70+6, 100) = 76
		},
		{
			name:             "500GB boot disk only",
			bootDiskGiB:      500,
			totalSSDGiB:      0,
			localSSDCount:    0,
			expectedEviction: 50,  // 10% of 500GB
			expectedSystem:   100, // min(250, 175+6, 100) = 100
		},
		// Local SSD scenarios
		{
			name:             "1 SSD - 375GB",
			bootDiskGiB:      100,
			totalSSDGiB:      375,
			localSSDCount:    1,
			expectedEviction: 37,  // 10% of 375GB (rounded down)
			expectedSystem:   50,  // 1 SSD = 50GB
		},
		{
			name:             "2 SSDs - 750GB",
			bootDiskGiB:      100,
			totalSSDGiB:      750,
			localSSDCount:    2,
			expectedEviction: 75,  // 10% of 750GB
			expectedSystem:   75,  // 2 SSDs = 75GB
		},
		{
			name:             "4 SSDs - 1500GB",
			bootDiskGiB:      100,
			totalSSDGiB:      1500,
			localSSDCount:    4,
			expectedEviction: 150, // 10% of 1500GB
			expectedSystem:   100, // 3+ SSDs = 100GB
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eviction, system := ResolveReservedEphemeralStorage(tt.bootDiskGiB, tt.totalSSDGiB, tt.localSSDCount)
			assert.Equal(t, tt.expectedEviction, eviction, "eviction threshold mismatch")
			assert.Equal(t, tt.expectedSystem, system, "system reservation mismatch")
		})
	}
}

func TestResolveReservedResource(t *testing.T) {
	tests := []struct {
		name             string
		instanceType     string
		cpuMCore         int64
		memoryMiB        int64
		bootDiskGiB      int64
		totalSSDGiB      int64
		localSSDCount    int64
		expectedCPU      int64
		expectedMemory   int64
		expectedEvict    int64
		expectedEphEvict int64
		expectedEphSys   int64
	}{
		{
			name:             "e2-micro with boot disk",
			instanceType:     "e2-micro",
			cpuMCore:         1000,
			memoryMiB:        1024,
			bootDiskGiB:      100,
			totalSSDGiB:      0,
			localSSDCount:    0,
			expectedCPU:      1060,
			expectedMemory:   255,
			expectedEvict:    100,
			expectedEphEvict: 10,
			expectedEphSys:   41, // min(50, 35+6, 100) = 41
		},
		{
			name:             "n2-standard-4 with 1 SSD",
			instanceType:     "n2-standard-4",
			cpuMCore:         4000,
			memoryMiB:        16384,
			bootDiskGiB:      100,
			totalSSDGiB:      375,
			localSSDCount:    1,
			expectedCPU:      80,
			expectedMemory:   2662,
			expectedEvict:    100,
			expectedEphEvict: 37,
			expectedEphSys:   50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, memory, evict, ephEvict, ephSys := ResolveReservedResource(
				tt.instanceType, tt.cpuMCore, tt.memoryMiB, tt.bootDiskGiB, tt.totalSSDGiB, tt.localSSDCount)
			
			assert.Equal(t, tt.expectedCPU, cpu, "CPU reservation mismatch")
			assert.Equal(t, tt.expectedMemory, memory, "Memory reservation mismatch")
			assert.Equal(t, tt.expectedEvict, evict, "Memory eviction mismatch")
			assert.Equal(t, tt.expectedEphEvict, ephEvict, "Ephemeral eviction mismatch")
			assert.Equal(t, tt.expectedEphSys, ephSys, "Ephemeral system mismatch")
		})
	}
}
