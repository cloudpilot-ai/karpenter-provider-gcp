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

package metadata

import (
	"encoding/json"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func RenderKubeletConfigMetadata(values MetadataValues, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, capacityType string) error {
	configStr, ok := values[KubeletConfigLabel]
	if !ok {
		return errors.New("kubelet-config metadata not found")
	}
	cpuMilliCore := fmt.Sprintf("%dm", instanceType.Overhead.KubeReserved.Cpu().MilliValue())
	memoryMB := fmt.Sprintf("%dMi", instanceType.Overhead.KubeReserved.Memory().Value()/(1024*1024))
	ephemeralStorage := instanceType.Overhead.KubeReserved.StorageEphemeral().String()

	if configStr == "" {
		return errors.New("kubelet-config metadata is empty")
	}

	// Parse YAML
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
		return fmt.Errorf("failed to parse kubelet-config YAML: %w", err)
	}

	// Update kubeReserved with provider-computed defaults first; user values
	// in spec.kubeletConfiguration.kubeReserved overlay these per-sub-key
	// below via mergeUserKubeletConfig.
	kubeReserved, ok := config["kubeReserved"].(map[string]interface{})
	if !ok {
		kubeReserved = make(map[string]interface{})
	}
	kubeReserved["cpu"] = cpuMilliCore
	kubeReserved["memory"] = memoryMB
	kubeReserved["ephemeral-storage"] = ephemeralStorage
	config["kubeReserved"] = kubeReserved

	// Merge user-supplied KubeletConfiguration on top of the provider defaults.
	// Map fields (kubeReserved, systemReserved, eviction*) merge one level
	// deep so user sub-keys win but computed sub-keys for unset keys survive
	// (e.g. computed kubeReserved.ephemeral-storage from boot disk size).
	if err := mergeUserKubeletConfig(config, nodeClass.Spec.KubeletConfiguration); err != nil {
		return fmt.Errorf("failed to merge user KubeletConfiguration: %w", err)
	}

	// Spot-specific overrides are applied AFTER the user merge: graceful
	// node shutdown is required for Spot preemption handling and must not
	// be overridable from the nodeclass.
	if capacityType == karpv1.CapacityTypeSpot {
		featureGates, ok := config["featureGates"].(map[string]interface{})
		if !ok {
			featureGates = make(map[string]interface{})
		}
		featureGates["GracefulNodeShutdown"] = true
		config["featureGates"] = featureGates
		config["shutdownGracePeriod"] = "30s"
		config["shutdownGracePeriodCriticalPods"] = "15s"
	}

	// Marshal back to YAML
	updatedYAML, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal updated kubelet-config YAML: %w", err)
	}

	values[KubeletConfigLabel] = string(updatedYAML)

	return nil
}

// mergeUserKubeletConfig overlays user KubeletConfiguration onto config.
// Scalar/slice keys replace verbatim; map keys merge one level deep so
// provider-computed sub-keys (e.g. kubeReserved.ephemeral-storage from #220)
// survive a partial user override.
func mergeUserKubeletConfig(config map[string]interface{}, kc *v1alpha1.KubeletConfiguration) error {
	if kc == nil {
		return nil
	}
	raw, err := json.Marshal(kc)
	if err != nil {
		return fmt.Errorf("marshal KubeletConfiguration: %w", err)
	}
	userMap := map[string]interface{}{}
	if err := json.Unmarshal(raw, &userMap); err != nil {
		return fmt.Errorf("unmarshal KubeletConfiguration: %w", err)
	}
	for k, v := range userMap {
		if userSub, ok := v.(map[string]interface{}); ok {
			if existingSub, ok := config[k].(map[string]interface{}); ok {
				for sk, sv := range userSub {
					existingSub[sk] = sv
				}
				continue
			}
		}
		config[k] = v
	}
	return nil
}
