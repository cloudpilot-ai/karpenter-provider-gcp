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
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/disktype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/localssd"
)

// staticKubeProxyCPUMilliCore matches the CPU request on GKE's node-owned
// kube-proxy mirror pod observed on GKE 1.35.5 COS ARM64 Karpenter nodes.
// Re-check this constant when GKE changes kube-proxy resource requests.
const staticKubeProxyCPUMilliCore = 100

func NewInstanceType(ctx context.Context, mt *computepb.MachineType, nodeClass *v1alpha1.GCENodeClass,
	region string, offerings cloudprovider.Offerings, ssdCount int) *cloudprovider.InstanceType {
	if offerings == nil {
		return nil
	}

	it := NewStaticInstanceType(ctx, mt, nodeClass, ssdCount)
	if it == nil {
		return nil
	}
	it.Requirements = computeRequirements(mt, offerings, region, ssdCount)
	it.Offerings = offerings
	return it
}

func NewStaticInstanceType(ctx context.Context, mt *computepb.MachineType, nodeClass *v1alpha1.GCENodeClass, ssdCount int) *cloudprovider.InstanceType {
	bootDiskGiB, totalSSDGiB := calculateDiskConfigGiB(nodeClass, mt, ssdCount)

	// RawBlock SSDs do not contribute to kubelet ephemeral storage.
	kubeletEphemeralSSDGiB, kubeletEphemeralSSDCount := totalSSDGiB, ssdCount
	if nodeClass.Spec.LocalSsdMode != v1alpha1.LocalSSDModeEphemeral {
		kubeletEphemeralSSDGiB, kubeletEphemeralSSDCount = 0, 0
	}

	totalStorageGiB := kubeletEphemeralSSDGiB
	if totalStorageGiB == 0 {
		totalStorageGiB = bootDiskGiB
	}
	totalStorageBytes := totalStorageGiB * 1024 * 1024 * 1024

	reservedCPU, reservedMemory, evictionMemory, ephemeralEviction, ephemeralSystem := utils.ResolveReservedResource(
		lo.FromPtr(mt.Name),
		int64(mt.GetGuestCpus()*1000),
		int64(mt.GetMemoryMb()),
		bootDiskGiB,
		kubeletEphemeralSSDGiB,
		int64(kubeletEphemeralSSDCount),
	)

	log.FromContext(ctx).V(1).Info("calculated ephemeral storage reservations",
		"instanceType", lo.FromPtr(mt.Name),
		"localSsdMode", nodeClass.Spec.LocalSsdMode,
		"bootDiskGiB", bootDiskGiB,
		"totalSSDGiB", totalSSDGiB,
		"localSSDCount", ssdCount,
		"kubeletEphemeralSSDGiB", kubeletEphemeralSSDGiB,
		"kubeletEphemeralSSDCount", kubeletEphemeralSSDCount,
		"ephemeralEviction", ephemeralEviction,
		"ephemeralSystem", ephemeralSystem)

	kc := nodeClass.Spec.KubeletConfiguration

	computedKubeReserved := corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewMilliQuantity(reservedCPU, resource.DecimalSI),
		corev1.ResourceMemory:           *resource.NewQuantity(reservedMemory*1024*1024, resource.BinarySI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(ephemeralSystem*1024*1024*1024, resource.BinarySI),
	}
	// GKE creates a node-owned kube-proxy mirror pod on Karpenter nodes. The provider
	// no longer injects the kube-proxy DaemonSet readiness label, so only the mirror
	// pod runs. Mirror pods are not part of Karpenter's daemon overhead simulation,
	// so account for the mirror pod's CPU request here as provider overhead. Using
	// SystemReserved (rather than KubeReserved) keeps this value out of kubelet
	// kubeReserved metadata, avoiding a double reduction of allocatable on top of the
	// mirror pod's own scheduling request.
	computedSystemReserved := corev1.ResourceList{
		corev1.ResourceCPU: *resource.NewMilliQuantity(staticKubeProxyCPUMilliCore, resource.DecimalSI),
	}
	computedEviction := corev1.ResourceList{
		corev1.ResourceMemory:           *resource.NewQuantity(evictionMemory*1024*1024, resource.BinarySI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(ephemeralEviction*1024*1024*1024, resource.BinarySI),
	}
	memoryQuantity := memory(ctx, mt)
	storageQuantity := resource.NewQuantity(totalStorageBytes, resource.BinarySI)

	overhead := cloudprovider.InstanceTypeOverhead{
		KubeReserved:      mergeKubeReserved(computedKubeReserved, kc),
		SystemReserved:    kcSystemReserved(computedSystemReserved, kc),
		EvictionThreshold: evictionThreshold(memoryQuantity, storageQuantity, computedEviction, kc),
	}

	it := &cloudprovider.InstanceType{
		Name:         lo.FromPtr(mt.Name),
		Requirements: scheduling.NewRequirements(),
		Capacity:     computeCapacity(ctx, mt, nodeClass, totalStorageBytes),
		Overhead:     &overhead,
	}

	return it
}

//nolint:gocyclo
func computeRequirements(mt *computepb.MachineType, offerings cloudprovider.Offerings, region string, ssdCount int) scheduling.Requirements {
	requirements := scheduling.NewRequirements(
		// Well Known Upstream
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, lo.FromPtr(mt.Name)),
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
		scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, fmt.Sprintf("%d", mt.GetMemoryMb())),
		scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceShape, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGeneration, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceSize, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUManufacturer, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUMemory, corev1.NodeSelectorOpDoesNotExist),
		scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpDoesNotExist),
	)
	requirements.Add(localSSDCountRequirement(ssdCount))
	for _, label := range disktype.AllLabels() {
		requirements.Add(scheduling.NewRequirement(label, corev1.NodeSelectorOpDoesNotExist))
	}
	if diskLabels, ok := disktype.LabelsForInstanceType(lo.FromPtr(mt.Name)); ok {
		for label := range diskLabels {
			requirements.Get(label).Insert("true")
		}
	}

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
		gpuName := extractGPUName(mt)
		requirements.Get(v1alpha1.LabelInstanceGPUName).Insert(gpuName)
		requirements.Get(v1alpha1.LabelInstanceGPUCount).Insert(fmt.Sprintf("%d", len(mt.GetAccelerators())))
		// LabelGKEAccelerator is required by the NVIDIA device plugin DaemonSet's nodeAffinity;
		// without it the device plugin will not schedule onto this node.
		requirements.Get(v1alpha1.LabelGKEAccelerator).Insert(gpuName)
	}

	// The format looks like: n1-standard-1, the family is n1, the instance size is 1.
	// Also, there is something like e2-medium, the family is e2, the instance size is medium.
	instanceTypeParts := strings.Split(lo.FromPtr(mt.Name), "-")
	if len(instanceTypeParts) >= 2 {
		sizeOffset := 1
		if len(instanceTypeParts) > 3 {
			sizeOffset = 2
		}
		requirements.Get(v1alpha1.LabelInstanceSize).Insert(instanceTypeParts[len(instanceTypeParts)-sizeOffset])
		// The laster number of the first part is the generation
		requirements.Get(v1alpha1.LabelInstanceGeneration).Insert(extractGeneration(instanceTypeParts[0]))
		requirements.Get(v1alpha1.LabelInstanceFamily).Insert(instanceTypeParts[0])
		requirements.Get(v1alpha1.LabelInstanceShape).Insert(instanceTypeParts[1])

		requirements.Get(corev1.LabelArchStable).Insert(machineTypeArch(mt))
	}

	return requirements
}

func localSSDCountRequirement(ssdCount int) *scheduling.Requirement {
	return scheduling.NewRequirement(
		v1alpha1.LabelInstanceLocalSsdCount,
		corev1.NodeSelectorOpIn,
		fmt.Sprintf("%d", ssdCount),
	)
}

func extractGPUName(mt *computepb.MachineType) string {
	if len(mt.GetAccelerators()) > 0 {
		return mt.GetAccelerators()[0].GetGuestAcceleratorType()
	}
	return ""
}

func extractGeneration(instanceTypePrefix string) string {
	offset := 1
	if len(instanceTypePrefix) == 3 {
		offset = 2
	}
	return string(instanceTypePrefix[len(instanceTypePrefix)-offset])
}

func machineTypeArch(mt *computepb.MachineType) string {
	switch mt.GetArchitecture() {
	case "ARM64":
		return "arm64"
	case "X86_64":
		return "amd64"
	default:
		return "amd64"
	}
}

func computeCapacity(ctx context.Context, mt *computepb.MachineType, nodeClass *v1alpha1.GCENodeClass, totalStorageBytes int64) corev1.ResourceList {
	maxPods := kcMaxPods(nodeClass.GetMaxPods(), nodeClass.Spec.KubeletConfiguration, int64(mt.GetGuestCpus()))
	resourceList := corev1.ResourceList{
		corev1.ResourceCPU:              *cpu(mt),
		corev1.ResourceMemory:           *memory(ctx, mt),
		corev1.ResourcePods:             *resource.NewQuantity(maxPods, resource.DecimalSI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(totalStorageBytes, resource.BinarySI),
		v1alpha1.ResourceNVIDIAGPU:      *resource.NewQuantity(int64(len(mt.GetAccelerators())), resource.DecimalSI),
	}
	return resourceList
}

func cpu(mt *computepb.MachineType) *resource.Quantity {
	return resource.NewQuantity(int64(mt.GetGuestCpus()), resource.DecimalSI)
}

func memory(ctx context.Context, mt *computepb.MachineType) *resource.Quantity {
	osReservedPercent := options.FromContext(ctx).VMMemoryOverheadPercent
	totalQuantity := int64(mt.GetMemoryMb()) * 1024 * 1024
	return resource.NewQuantity(totalQuantity-int64(float64(totalQuantity)*osReservedPercent), resource.DecimalSI)
}

func calculateDiskConfigGiB(nodeClass *v1alpha1.GCENodeClass, mt *computepb.MachineType, ssdCount int) (int64, int64) {
	bootDiskGiB := int64(100) // Default boot disk size
	if nodeClass != nil {
		for _, disk := range nodeClass.Spec.Disks {
			if disk.Boot && disk.SizeGiB > 0 {
				bootDiskGiB = int64(disk.SizeGiB)
				break
			}
		}
	}
	totalSSDGiB := localssd.TotalGiB(lo.FromPtr(mt.Name), ssdCount)
	return bootDiskGiB, totalSSDGiB
}
