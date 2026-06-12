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

package metadata

import kubeletconfig "k8s.io/kubelet/config/v1beta1"

func (m *InstanceMetadata) KubeLabels() Labels {
	if m == nil {
		return nil
	}
	return m.kubeLabels
}

func (m *InstanceMetadata) KubeletConfig() kubeletconfig.KubeletConfiguration {
	if m == nil {
		return kubeletconfig.KubeletConfiguration{}
	}
	return m.kubeletConfig
}

func (m *InstanceMetadata) CustomMetadata() CustomMetadata {
	if m == nil {
		return nil
	}
	return m.customMetadata
}

func (m *InstanceMetadata) InstanceLabels() InstanceLabels {
	if m == nil {
		return nil
	}
	return m.instanceLabels
}

func (e *KubeEnv) Entries() KubeEnvEntries {
	if e == nil {
		return nil
	}
	return e.entries
}
