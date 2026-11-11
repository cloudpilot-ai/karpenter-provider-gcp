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

package instancetype

import (
	"fmt"
	"strings"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/disktype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

// DefaultBootDiskGiB is the boot disk the provider creates for a swap-enabled
// node class that configures no boot disk. A swap file needs a disk the
// provider sizes, and GKE's documented default boot disk is 100 GiB, which is
// also the size the provider's ephemeral-storage estimate already assumes.
const DefaultBootDiskGiB int64 = 100

// defaultBootDiskTypes are the disk types the provider creates a boot disk with,
// in order of preference, restricted to the machine family's supported types.
// Machine families differ: hyperdisk-only families cannot boot from pd-standard.
var defaultBootDiskTypes = []v1alpha1.DiskCategory{"pd-standard", "pd-balanced", "hyperdisk-balanced"}

// BootDiskSwap is the boot disk and the swap file the provider renders for one
// candidate machine.
type BootDiskSwap struct {
	// DefaultBootDisk is the provider-created boot disk to attach when the node
	// class configures no boot disk; nil otherwise.
	DefaultBootDisk *v1alpha1.Disk
	// BootDiskGiB is the size of the boot disk the provider renders.
	BootDiskGiB int64
	// SwapGiB is the resolved swap file size, 0 when boot-disk swap is disabled.
	SwapGiB int64
}

// ResolveBootDiskSwap decides the boot disk the provider renders for a candidate
// machine and the swap file the node creates on it.
//
// machineMemoryMiB is the raw Compute memory of the machine type. A node class
// without boot-disk swap keeps the previous disk behavior: an explicit boot disk
// size, or the estimator's 100 GiB fallback. With boot-disk swap enabled, the
// provider must know the boot disk to enforce the half-disk limit, so it creates
// a default boot disk when the node class configures none.
func ResolveBootDiskSwap(nodeClass *v1alpha1.GCENodeClass, instanceType string, machineMemoryMiB int64) (BootDiskSwap, error) {
	if nodeClass == nil {
		return BootDiskSwap{BootDiskGiB: DefaultBootDiskGiB}, nil
	}
	bootDiskGiB, explicit, err := explicitBootDiskGiB(nodeClass)
	if err != nil {
		return BootDiskSwap{}, err
	}
	swap := nodeClass.BootDiskSwapConfig()
	if swap == nil {
		if !explicit {
			bootDiskGiB = DefaultBootDiskGiB
		}
		return BootDiskSwap{BootDiskGiB: bootDiskGiB}, nil
	}

	resolved := BootDiskSwap{}
	if !explicit {
		category, err := defaultBootDiskType(instanceType)
		if err != nil {
			return BootDiskSwap{}, err
		}
		resolved.DefaultBootDisk = &v1alpha1.Disk{
			SizeGiB:  int32(DefaultBootDiskGiB),
			Category: category,
			Boot:     true,
		}
		bootDiskGiB = DefaultBootDiskGiB
	}
	resolved.BootDiskGiB = bootDiskGiB

	swapGiB, err := utils.BootDiskSwapGiB(machineMemoryMiB, bootDiskGiB, nodeClass.BootDiskSwapRequestedGiB())
	if err != nil {
		return BootDiskSwap{}, fmt.Errorf("boot-disk swap for machine type %q: %w", instanceType, err)
	}
	resolved.SwapGiB = swapGiB
	return resolved, nil
}

// explicitBootDiskGiB returns the size of the boot disk the node class
// configures. ok is false when it configures none.
func explicitBootDiskGiB(nodeClass *v1alpha1.GCENodeClass) (sizeGiB int64, ok bool, err error) {
	var bootDisks []v1alpha1.Disk
	for _, disk := range nodeClass.Spec.Disks {
		if disk.Boot {
			bootDisks = append(bootDisks, disk)
		}
	}
	if len(bootDisks) == 0 {
		return 0, false, nil
	}
	if nodeClass.BootDiskSwapConfig() == nil {
		// Without a swap file the boot disk size does not affect provider
		// decisions, so keep accepting a disk whose size GCE resolves.
		for _, disk := range bootDisks {
			if disk.SizeGiB > 0 {
				return int64(disk.SizeGiB), true, nil
			}
		}
		return 0, false, nil
	}
	if len(bootDisks) > 1 {
		return 0, false, fmt.Errorf("boot-disk swap requires exactly one boot disk, but the node class configures %d", len(bootDisks))
	}
	if bootDisks[0].SizeGiB <= 0 {
		return 0, false, fmt.Errorf("boot-disk swap requires a boot disk with an explicit sizeGiB, because the half-disk limit cannot be checked otherwise")
	}
	return int64(bootDisks[0].SizeGiB), true, nil
}

// defaultBootDiskType returns the disk type the provider creates a boot disk
// with for the machine family: pd-standard where the family supports it, then
// pd-balanced, then hyperdisk-balanced. A family with no known disk
// compatibility fails rather than guessing a type GCE may reject.
func defaultBootDiskType(instanceType string) (v1alpha1.DiskCategory, error) {
	labels, ok := disktype.LabelsForInstanceType(instanceType)
	if !ok {
		return "", fmt.Errorf("cannot choose a default boot disk type for machine type %q: no disk type compatibility is known for machine family %q",
			instanceType, disktype.Family(instanceType))
	}
	for _, diskType := range defaultBootDiskTypes {
		if disktype.SupportsDiskType(instanceType, string(diskType)) {
			return diskType, nil
		}
	}
	supported := make([]string, 0, len(labels))
	for label := range labels {
		supported = append(supported, strings.TrimPrefix(label, "disk-type.gke.io/"))
	}
	return "", fmt.Errorf("cannot choose a default boot disk type for machine type %q: machine family %q supports %s, none of %v",
		instanceType, disktype.Family(instanceType), strings.Join(supported, ", "), defaultBootDiskTypes)
}
