/*
Copyright 2026 The CloudPilot AI Authors.

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

	"github.com/stretchr/testify/require"
)

// TestBootDiskSwapGiBAutomaticCases pins every size observed on GKE
// 1.35.8-gke.1796000. The expectations are the recorded
// observations, not values this package computes, so a change to the sizing
// policy fails here instead of silently agreeing with itself.
func TestBootDiskSwapGiBAutomaticCases(t *testing.T) {
	for _, tc := range []struct {
		name             string
		machineMemoryMiB int64
		bootDiskGiB      int64
		wantGiB          int64
	}{
		{name: "2GiB memory, 100GiB disk", machineMemoryMiB: 2048, bootDiskGiB: 100, wantGiB: 4},
		{name: "4GiB memory, 100GiB disk", machineMemoryMiB: 4096, bootDiskGiB: 100, wantGiB: 8},
		{name: "8GiB memory, 100GiB disk", machineMemoryMiB: 8192, bootDiskGiB: 100, wantGiB: 16},
		{name: "16GiB memory, 100GiB disk", machineMemoryMiB: 16384, bootDiskGiB: 100, wantGiB: 24},
		{name: "32GiB memory, 200GiB disk", machineMemoryMiB: 32768, bootDiskGiB: 200, wantGiB: 40},
		{name: "64GiB memory, 400GiB disk", machineMemoryMiB: 65536, bootDiskGiB: 400, wantGiB: 72},
		// 7.75GiB truncates to 7GiB before doubling, so the result is 14GiB, not
		// floor(2*7.75)=15GiB.
		{name: "7.75GiB custom memory", machineMemoryMiB: 7936, bootDiskGiB: 100, wantGiB: 14},
		{name: "8.25GiB custom memory", machineMemoryMiB: 8448, bootDiskGiB: 100, wantGiB: 16},
		{name: "8GiB memory capped by a 20GiB disk", machineMemoryMiB: 8192, bootDiskGiB: 20, wantGiB: 10},
		{name: "16GiB memory capped by a 21GiB disk", machineMemoryMiB: 16384, bootDiskGiB: 21, wantGiB: 10},
		// Ubuntu accepts the same automatic size; only the encryption metadata
		// differs, which this sizing policy does not see.
		{name: "Ubuntu 16GiB memory, no encryption", machineMemoryMiB: 16384, bootDiskGiB: 100, wantGiB: 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BootDiskSwapGiB(tc.machineMemoryMiB, tc.bootDiskGiB, 0)
			require.NoError(t, err)
			require.Equal(t, tc.wantGiB, got)
		})
	}
}

func TestBootDiskSwapGiBExplicitSize(t *testing.T) {
	got, err := BootDiskSwapGiB(16384, 100, 4)
	require.NoError(t, err)
	require.Equal(t, int64(4), got, "an explicit size is used unchanged")

	got, err = BootDiskSwapGiB(0, 100, 4)
	require.NoError(t, err, "an explicit size does not need the machine's memory")
	require.Equal(t, int64(4), got)

	got, err = BootDiskSwapGiB(16384, 100, 50)
	require.NoError(t, err, "half of the boot disk is allowed")
	require.Equal(t, int64(50), got)

	_, err = BootDiskSwapGiB(16384, 100, 51)
	require.ErrorContains(t, err, "more than half of the 100GiB boot disk")

	_, err = BootDiskSwapGiB(16384, 100, -1)
	require.ErrorContains(t, err, "at least 1GiB")
}

func TestBootDiskSwapGiBRejectsUnresolvableDisks(t *testing.T) {
	_, err := BootDiskSwapGiB(2048, 0, 0)
	require.ErrorContains(t, err, "boot disk size must be positive")

	_, err = BootDiskSwapGiB(2048, 1, 0)
	require.ErrorContains(t, err, "cannot fit a boot-disk swap file on a 1GiB boot disk")

	_, err = BootDiskSwapGiB(1023, 100, 0)
	require.ErrorContains(t, err, "cannot derive a boot-disk swap size from 1023MiB of machine memory")

	_, err = BootDiskSwapGiB(0, 100, 0)
	require.ErrorContains(t, err, "cannot derive a boot-disk swap size from 0MiB of machine memory")
}
