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

package instancetype

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func NewInstanceType(ctx context.Context, mt *computepb.MachineType, nodeClass *v1alpha1.GCENodeClass,
	region string, offerings cloudprovider.Offerings) *cloudprovider.InstanceType {
	if offerings == nil {
		return nil
	}

	reservedCPU, reservedMemory, evictionMemory := utils.ResolveReservedResource(aws.StringValue(mt.Name), int64(mt.GetGuestCpus()*1000), int64(mt.GetMemoryMb()))
	overhead := cloudprovider.InstanceTypeOverhead{
		KubeReserved: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(reservedCPU, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(reservedMemory*1024*1024, resource.BinarySI),
		},
		EvictionThreshold: corev1.ResourceList{
			corev1.ResourceMemory: *resource.NewQuantity(evictionMemory*1024*1024, resource.BinarySI),
		},
		SystemReserved: corev1.ResourceList{},
	}

	it := &cloudprovider.InstanceType{
		Name:         aws.StringValue(mt.Name),
		Requirements: computeRequirements(mt, offerings, region),
		Offerings:    offerings,
		Capacity:     computeCapacity(ctx, mt, nodeClass),
		Overhead:     &overhead,
	}

	return it
}

func extractCategory(part string) string {
	i := 0
	for ; i < len(part); i++ {
		if part[i] >= '0' && part[i] <= '9' {
			break
		}
	}
	return part[:i]
}

//nolint:gocyclo
func computeRequirements(mt *computepb.MachineType, offerings cloudprovider.Offerings, region string) scheduling.Requirements {
	requirements := scheduling.NewRequirements(
		// Well Known Upstream
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, aws.StringValue(mt.Name)),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, lo.Map(offerings.Available(), func(o *cloudprovider.Offering, _ int) string {
			return o.Requirements.Get(corev1.LabelTopologyZone).Any()
		})...),
		scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region),
		scheduling.NewRequirement(corev1.LabelWindowsBuild, corev1.NodeSelectorOpDoesNotExist),

		// Well Known to Karpenter
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, lo.Map(offerings.Available(), func(o *cloudprovider.Offering, _ int) string {
			return o.Requirements.Get(karpv1.CapacityTypeLabelKey).Any()
		})...),

		// Well Known to Google Cloud
		scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, fmt.Sprintf("%d", mt.GetGuestCpus())),
		scheduling.NewRequirement(v1alpha1.LabelInstanceCPUModel, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, fmt.Sprintf("%d", mt.GetMemoryMb())),
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
	if zoneIDs := lo.FilterMap(offerings.Available(), func(o *cloudprovider.Offering, _ int) (string, bool) {
		zoneID := o.Requirements.Get(v1alpha1.LabelTopologyZoneID).Any()
		return zoneID, zoneID != ""
	}); len(zoneIDs) != 0 {
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelTopologyZoneID, corev1.NodeSelectorOpIn, zoneIDs...))
	}

	// GPU labels
	if len(mt.GetAccelerators()) > 0 {
		requirements.Get(v1alpha1.LabelInstanceGPUName).Insert(extractGPUName(mt))
		requirements.Get(v1alpha1.LabelInstanceGPUCount).Insert(fmt.Sprintf("%d", len(mt.GetAccelerators())))
	}

	// The format looks like: n1-standard-1, the family is n1-standard, the category is n, the instance size is 1
	// Also, there is something like e2-medium, the family is e2, the category is e, the instance size is medium
	instanceTypeParts := strings.Split(aws.StringValue(mt.Name), "-")
	if len(instanceTypeParts) >= 2 {
		requirements.Get(v1alpha1.LabelInstanceCategory).Insert(extractCategory(instanceTypeParts[0]))
		requirements.Get(v1alpha1.LabelInstanceSize).Insert(instanceTypeParts[len(instanceTypeParts)-1])
		// The laster number of the first part is the generation
		requirements.Get(v1alpha1.LabelInstanceGeneration).Insert(extractGeneration(instanceTypeParts[0]))

		if len(instanceTypeParts) == 2 {
			requirements.Get(v1alpha1.LabelInstanceFamily).Insert(instanceTypeParts[0])
		}
		// If there are three parts, also insert the first two joined as family
		if len(instanceTypeParts) == 3 {
			requirements.Get(v1alpha1.LabelInstanceFamily).Insert(instanceTypeParts[0] + "-" + instanceTypeParts[1])
		}

		requirements.Get(corev1.LabelArchStable).Insert(extractArch(instanceTypeParts[0]))
	}

	return requirements
}

func extractGPUName(mt *computepb.MachineType) string {
	if len(mt.GetAccelerators()) > 0 {
		return mt.GetAccelerators()[0].GetGuestAcceleratorType()
	}
	return ""
}

func extractGeneration(instanceTypePrefix string) string {
	// The laster number of the first part is the generation
	return string(instanceTypePrefix[len(instanceTypePrefix)-1])
}

func extractArch(instanceTypePrefix string) string {
	// referring to https://cloud.google.com/compute/docs/instances/arm-on-compute
	if instanceTypePrefix == "a4x" || instanceTypePrefix == "c4a" || instanceTypePrefix == "t2a" {
		return "arm64"
	}
	return "amd64"
}

func computeCapacity(ctx context.Context, mt *computepb.MachineType, nodeClass *v1alpha1.GCENodeClass) corev1.ResourceList {
	resourceList := corev1.ResourceList{
		corev1.ResourceCPU:    *cpu(mt),
		corev1.ResourceMemory: *memory(ctx, mt),
		corev1.ResourcePods:   *pods(nodeClass),
	}
	return resourceList
}

func pods(nodeClass *v1alpha1.GCENodeClass) *resource.Quantity {
	if nodeClass.Spec.KubeletConfiguration != nil && nodeClass.Spec.KubeletConfiguration.MaxPods != nil {
		return resource.NewQuantity(int64(*nodeClass.Spec.KubeletConfiguration.MaxPods), resource.DecimalSI)
	}
	return resource.NewQuantity(int64(v1alpha1.KubeletMaxPods), resource.DecimalSI)
}

func cpu(mt *computepb.MachineType) *resource.Quantity {
	return resource.NewQuantity(int64(mt.GetGuestCpus()), resource.DecimalSI)
}

func memory(ctx context.Context, mt *computepb.MachineType) *resource.Quantity {
	osReservedPercent := options.FromContext(ctx).VMMemoryOverheadPercent
	totalQuantity := int64(mt.GetMemoryMb()) * 1024 * 1024
	return resource.NewQuantity(totalQuantity-int64(float64(totalQuantity)*osReservedPercent), resource.DecimalSI)
}
