/*
Copyright 2024 The CloudPilot AI Authors.

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
	"context"
	"fmt"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func NewInstanceType(ctx context.Context,
	mt *computepb.MachineType, kc *v1alpha1.KubeletConfiguration, region string, disk *v1alpha1.Disk,
	offerings cloudprovider.Offerings, clusterCNI string) *cloudprovider.InstanceType {
	if offerings == nil {
		return nil
	}

	it := &cloudprovider.InstanceType{
		Name:         *mt.Name,
		Requirements: computeRequirements(mt, offerings, region),
		Offerings:    offerings,
		Capacity:     computeCapacity(ctx, mt),
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved:      corev1.ResourceList{},
			SystemReserved:    corev1.ResourceList{},
			EvictionThreshold: corev1.ResourceList{},
		},
	}

	// TODO: update with reserved api.

	return it
}

//nolint:gocyclo
func computeRequirements(mt *computepb.MachineType, offerings cloudprovider.Offerings, region string) scheduling.Requirements {
	requirements := scheduling.NewRequirements(
		// Well Known Upstream
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, *mt.Name),
		// TODO: fix this with available arch
		// scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, v1alpha1.ArchitectureAMD64),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, lo.Map(offerings.Available(), func(o cloudprovider.Offering, _ int) string {
			return o.Requirements.Get(corev1.LabelTopologyZone).Any()
		})...),
		scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region),
		scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),
		// Well Known to Karpenter
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, lo.Map(offerings.Available(), func(o cloudprovider.Offering, _ int) string {
			return o.Requirements.Get(karpv1.CapacityTypeLabelKey).Any()
		})...),
		// Well Known to AlibabaCloud
		scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, string(mt.GetGuestCpus())),
		scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, fmt.Sprintf("%dMb", mt.GetMemoryMb())),
		scheduling.NewRequirement(v1alpha1.LabelInstanceCategory, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
	)
	// Only add zone-id label when available in offerings. It may not be available if a user has upgraded from a
	// previous version of Karpenter w/o zone-id support and the nodeclass vswitch status has not yet updated.
	if zoneIDs := lo.FilterMap(offerings.Available(), func(o cloudprovider.Offering, _ int) (string, bool) {
		zoneID := o.Requirements.Get(v1alpha1.LabelTopologyZoneID).Any()
		return zoneID, zoneID != ""
	}); len(zoneIDs) != 0 {
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelTopologyZoneID, corev1.NodeSelectorOpIn, zoneIDs...))
	}

	// TODO: Add Instance Type Labels

	return requirements
}

func computeCapacity(ctx context.Context, mt *computepb.MachineType) corev1.ResourceList {

	resourceList := corev1.ResourceList{
		corev1.ResourceCPU:    *cpu(mt),
		corev1.ResourceMemory: *memory(ctx, mt),
	}
	return resourceList
}

func cpu(mt *computepb.MachineType) *resource.Quantity {
	return resources.Quantity(fmt.Sprint(mt.GetGuestCpus()))
}

func extractMemory(mt *computepb.MachineType) *resource.Quantity {
	return resources.Quantity(fmt.Sprintf("%fMb", mt.GetMemoryMb()))
}

func memory(ctx context.Context, mt *computepb.MachineType) *resource.Quantity {
	mem := extractMemory(mt)
	if mem.IsZero() {
		return mem
	}
	return mem
}
