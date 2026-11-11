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
	"fmt"
)

// automaticSwapMemoryGiB is the amount of memory that an automatic boot-disk
// swap file duplicates: GKE sizes swap as twice the memory up to 8 GiB, then as
// the memory plus 8 GiB. The measured cases are in swap_test.go.
const automaticSwapMemoryGiB int64 = 8

// BootDiskSwapGiB resolves the size of the boot-disk swap file for one machine.
//
// machineMemoryMiB is the raw Compute memory of the selected machine type, not
// the schedulable memory that subtracts VM overhead: GKE resolves the automatic
// size from the machine's nominal memory. requestedGiB 0 selects the automatic
// size; a positive requestedGiB is validated against the half-disk limit that
// GKE enforces and never clamped.
//
// The automatic size is empirically derived from GKE 1.35.8-gke.1796000,
// not from a Google-published formula:
//
//	ramGiB = machineMemoryMiB / 1024                      (integer truncation first)
//	swapGiB = min(ramGiB + min(ramGiB, 8), bootDiskGiB/2)
func BootDiskSwapGiB(machineMemoryMiB, bootDiskGiB, requestedGiB int64) (int64, error) {
	if bootDiskGiB <= 0 {
		return 0, fmt.Errorf("boot disk size must be positive, got %dGiB", bootDiskGiB)
	}
	limit := bootDiskGiB / 2
	if requestedGiB != 0 {
		if requestedGiB < 1 {
			return 0, fmt.Errorf("swap size must be at least 1GiB, got %dGiB", requestedGiB)
		}
		if requestedGiB > limit {
			return 0, fmt.Errorf("swap size %dGiB is more than half of the %dGiB boot disk (%dGiB)", requestedGiB, bootDiskGiB, limit)
		}
		return requestedGiB, nil
	}
	ramGiB := machineMemoryMiB / 1024
	if ramGiB < 1 {
		return 0, fmt.Errorf("cannot derive a boot-disk swap size from %dMiB of machine memory", machineMemoryMiB)
	}
	if limit < 1 {
		return 0, fmt.Errorf("cannot fit a boot-disk swap file on a %dGiB boot disk", bootDiskGiB)
	}
	return min(ramGiB+min(ramGiB, automaticSwapMemoryGiB), limit), nil
}
