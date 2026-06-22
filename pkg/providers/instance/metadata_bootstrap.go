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

package instance

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

func patchKubeEnvOSDistribution(target *metadata.InstanceMetadata, nodeClass *v1alpha1.GCENodeClass) {
	switch nodeClass.ImageFamily() {
	case v1alpha1.ImageFamilyUbuntu:
		target.UnsetKubeEnvEntry("ENABLE_NODE_BFQ_IO_SCHEDULER")
		target.UnsetKubeEnvEntry("NODE_BFQ_IO_SCHEDULER_IO_WEIGHT")
	case v1alpha1.ImageFamilyContainerOptimizedOS:
		target.SetKubeEnvEntry("ENABLE_NODE_BFQ_IO_SCHEDULER", `"true"`)
		target.SetKubeEnvEntry("NODE_BFQ_IO_SCHEDULER_IO_WEIGHT", `"1200"`)
	}
}

func patchSecondaryBootDisksKubeEnv(target *metadata.InstanceMetadata, nodeClass *v1alpha1.GCENodeClass) {
	secondaryBootDiskPaths := []string{}
	for _, disk := range secondaryBootDisks(nodeClass) {
		secondaryBootDiskPaths = append(secondaryBootDiskPaths, fmt.Sprintf("/mnt/disks/gke-secondary-disks/%s", secondaryBootDiskImageDeviceName(disk.SecondaryBootImage)))
	}
	if len(secondaryBootDiskPaths) > 0 {
		target.SetKubeEnvEntry("SECONDARY_BOOT_DISKS", strings.Join(secondaryBootDiskPaths, ","))
	} else {
		target.UnsetKubeEnvEntry("SECONDARY_BOOT_DISKS")
	}
}

func applyInstanceTypeKubeReserved(config *kubeletconfig.KubeletConfiguration, instanceType *cloudprovider.InstanceType) {
	mergeStringMap(&config.KubeReserved, map[string]string{
		"cpu":               fmt.Sprintf("%dm", instanceType.Overhead.KubeReserved.Cpu().MilliValue()),
		"memory":            fmt.Sprintf("%dMi", instanceType.Overhead.KubeReserved.Memory().Value()/(1024*1024)),
		"ephemeral-storage": instanceType.Overhead.KubeReserved.StorageEphemeral().String(),
	})
}

func applySpotShutdownKubeletConfig(config *kubeletconfig.KubeletConfiguration, provisioningModel string) {
	if provisioningModel != karpv1.CapacityTypeSpot {
		return
	}
	if config.FeatureGates == nil {
		config.FeatureGates = map[string]bool{}
	}
	config.FeatureGates["GracefulNodeShutdown"] = true
	config.ShutdownGracePeriod.Duration = 30 * time.Second
	config.ShutdownGracePeriodCriticalPods.Duration = 15 * time.Second
}

func setProvisioningModelLabels(target *metadata.InstanceMetadata, provisioningModel string) {
	target.SetKubeLabel(GKEProvisioningLabel, provisioningModel)
	target.SetRegistrationNodeLabel(GKEProvisioningLabel, provisioningModel)
}

func applyNodeClassKubeletConfig(config *kubeletconfig.KubeletConfiguration, overlay *v1alpha1.KubeletConfiguration) {
	if config == nil || overlay == nil {
		return
	}
	if overlay.ClusterDNS != nil {
		config.ClusterDNS = append([]string{}, overlay.ClusterDNS...)
	}
	if overlay.MaxPods != nil {
		config.MaxPods = *overlay.MaxPods
	}
	if overlay.PodsPerCore != nil {
		config.PodsPerCore = *overlay.PodsPerCore
	}
	mergeKubeletQuantityMap(&config.SystemReserved, overlay.SystemReserved)
	mergeKubeletQuantityMap(&config.KubeReserved, overlay.KubeReserved)
	mergeKubeletQuantityMap(&config.EvictionHard, overlay.EvictionHard)
	mergeKubeletQuantityMap(&config.EvictionSoft, overlay.EvictionSoft)
	mergeDurationStringMap(&config.EvictionSoftGracePeriod, overlay.EvictionSoftGracePeriod)
	if overlay.EvictionMaxPodGracePeriod != nil {
		config.EvictionMaxPodGracePeriod = *overlay.EvictionMaxPodGracePeriod
	}
	if overlay.ImageGCHighThresholdPercent != nil {
		config.ImageGCHighThresholdPercent = overlay.ImageGCHighThresholdPercent
	}
	if overlay.ImageGCLowThresholdPercent != nil {
		config.ImageGCLowThresholdPercent = overlay.ImageGCLowThresholdPercent
	}
	if overlay.CPUCFSQuota != nil {
		config.CPUCFSQuota = overlay.CPUCFSQuota
	}
}

func mergeKubeletQuantityMap(target *map[string]string, overlay map[string]v1alpha1.KubeletQuantity) {
	if len(overlay) == 0 {
		return
	}
	values := make(map[string]string, len(overlay))
	for key, value := range overlay {
		values[key] = string(value)
	}
	mergeStringMap(target, values)
}

func mergeDurationStringMap(target *map[string]string, overlay map[string]metav1.Duration) {
	if len(overlay) == 0 {
		return
	}
	values := make(map[string]string, len(overlay))
	for key, value := range overlay {
		values[key] = value.Duration.String()
	}
	mergeStringMap(target, values)
}

func mergeStringMap(target *map[string]string, values map[string]string) {
	if len(values) == 0 {
		return
	}
	if *target == nil {
		*target = map[string]string{}
	}
	for key, value := range values {
		(*target)[key] = value
	}
}
