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
	"slices"

	"github.com/samber/lo"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// limitedSwapBehavior lets Burstable pods use the swap file, which is what GKE
// nodes with swap enabled run.
const limitedSwapBehavior = "LimitedSwap"

// swapImageFamilies are the node images whose bootstrap creates the boot-disk
// swap file. A custom image keeps its own bootstrap, so the provider cannot
// promise the swap file is created.
var swapImageFamilies = []string{v1alpha1.ImageFamilyContainerOptimizedOS, v1alpha1.ImageFamilyUbuntu}

// validateBootDiskSwapNodeClass rejects boot-disk swap configurations the
// provider cannot honor, before it renders an instance.
func validateBootDiskSwapNodeClass(nodeClass *v1alpha1.GCENodeClass) error {
	if nodeClass.BootDiskSwapConfig() == nil {
		return nil
	}
	if family := nodeClass.ImageFamily(); !slices.Contains(swapImageFamilies, family) {
		return fmt.Errorf("linuxNodeConfig.swapConfig is not supported for image family %q: only %v bootstrap the swap file", family, swapImageFamilies)
	}
	if kc := nodeClass.Spec.KubeletConfiguration; kc != nil && kc.FailSwapOn != nil && *kc.FailSwapOn {
		return fmt.Errorf("linuxNodeConfig.swapConfig conflicts with kubeletConfiguration.failSwapOn true: the kubelet does not start while swap is active")
	}
	return nil
}

// applyBootDiskSwapKubeletDefaults fills the kubelet settings that swap needs
// when the node class leaves them out. It runs after the node class overlay, so
// an explicit NoSwap or failSwapOn survives, and it replaces the source pool's
// values: a node that creates a swap file must not inherit settings from a pool
// that does not.
func applyBootDiskSwapKubeletDefaults(config *kubeletconfig.KubeletConfiguration, nodeClass *v1alpha1.GCENodeClass) {
	if nodeClass.BootDiskSwapConfig() == nil {
		return
	}
	kc := nodeClass.Spec.KubeletConfiguration
	if kc == nil || kc.FailSwapOn == nil {
		config.FailSwapOn = lo.ToPtr(false)
	}
	if kc == nil || kc.MemorySwap == nil || kc.MemorySwap.SwapBehavior == "" {
		config.MemorySwap.SwapBehavior = limitedSwapBehavior
	}
}

// applyBootDiskSwapReservation keeps the serialized kubelet ephemeral-storage
// reservation equal to the value the scheduler computes. The swap file is added
// to kubeReserved after the node class overlay, so an explicit
// kubeReserved.ephemeral-storage would otherwise drop the swap file from the
// kubelet's reservation while the scheduler still subtracts it.
func applyBootDiskSwapReservation(config *kubeletconfig.KubeletConfiguration, instanceType *cloudprovider.InstanceType, swapGiB int64) {
	if swapGiB == 0 {
		return
	}
	if config.KubeReserved == nil {
		config.KubeReserved = map[string]string{}
	}
	config.KubeReserved["ephemeral-storage"] = instanceType.Overhead.KubeReserved.StorageEphemeral().String()
}
