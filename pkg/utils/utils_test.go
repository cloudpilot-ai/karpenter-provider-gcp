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
