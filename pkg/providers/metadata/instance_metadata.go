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
	"context"
	"net/http"
	"sort"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

type MetadataValues map[string]string

type KubeEnv struct {
	value string
}

type KubeletConfig struct {
	value string
}

type InstanceLabels map[string]string

type InstanceMetadata struct {
	KubeEnv        KubeEnv
	KubeLabels     KubeLabels
	KubeletConfig  KubeletConfig
	InstanceLabels InstanceLabels
	CustomMetadata MetadataValues
}

func NewInstanceMetadataFromSource(source *compute.Metadata) *InstanceMetadata {
	values := FromAPI(source)
	return &InstanceMetadata{
		KubeEnv:        KubeEnv{value: values[KubeEnvKey]},
		KubeLabels:     ParseKubeLabels(values[KubeLabelsKey]),
		KubeletConfig:  KubeletConfig{value: values[KubeletConfigKey]},
		CustomMetadata: MetadataValues{},
	}
}

func (m *InstanceMetadata) ToComputeMetadata() *compute.Metadata {
	if m == nil {
		return MetadataValues{}.ToAPI()
	}
	return m.values().ToAPI()
}

func (m *InstanceMetadata) SetInstanceLabels(labels map[string]string) {
	m.InstanceLabels = InstanceLabels{}
	for key, value := range labels {
		m.InstanceLabels[key] = value
	}
}

func (m *InstanceMetadata) ToComputeLabels() map[string]string {
	labels := map[string]string{}
	if m == nil {
		return labels
	}
	for key, value := range m.InstanceLabels {
		labels[key] = value
	}
	return labels
}

func (m *InstanceMetadata) ReplaceBootstrapNodeLabels(kubeLabels, kubeEnvLabels map[string]string) error {
	return m.apply(func(values MetadataValues) error {
		return ReplaceNodeLabelsForSurfaces(values, kubeLabels, kubeEnvLabels)
	})
}

func (m *InstanceMetadata) SetMaxPods(maxPods int32) error {
	return m.apply(func(values MetadataValues) error { return SetMaxPods(values, maxPods) })
}

func (m *InstanceMetadata) SetKubeEnvZone(zone string) error {
	return m.apply(func(values MetadataValues) error { return SetKubeEnvZone(values, zone) })
}

func (m *InstanceMetadata) SetProvisioningModel(model string) error {
	return m.apply(func(values MetadataValues) error { return SetProvisioningModel(values, model) })
}

func (m *InstanceMetadata) ApplyUserMetadata(user map[string]string) {
	_ = m.apply(func(values MetadataValues) error {
		ApplyCustomMetadata(values, user)
		return nil
	})
}

func (m *InstanceMetadata) SetProviderNodeLabels(labels map[string]string) error {
	return m.apply(func(values MetadataValues) error { return SetNodeLabels(values, labels) })
}

func (m *InstanceMetadata) ReplaceProviderNodeLabels(labels map[string]string, shouldReplace func(string) bool) error {
	return m.apply(func(values MetadataValues) error { return ReplaceNodeLabels(values, labels, shouldReplace) })
}

func (m *InstanceMetadata) SetKubeEnvTaints(taints []string) error {
	return m.apply(func(values MetadataValues) error { return SetKubeEnvTaints(values, taints) })
}

func (m *InstanceMetadata) PatchKubeEnvTaints(taints []string) error {
	return m.apply(func(values MetadataValues) error { return PatchKubeEnvTaints(values, taints) })
}

func (m *InstanceMetadata) PatchKubeEnvForInstanceType(instanceType *cloudprovider.InstanceType) error {
	return m.apply(func(values MetadataValues) error { return PatchKubeEnvForInstanceType(values, instanceType) })
}

func (m *InstanceMetadata) PatchKubeEnvForOSType(imageFamily string) error {
	return m.apply(func(values MetadataValues) error { return PatchKubeEnvForOSType(values, imageFamily) })
}

func (m *InstanceMetadata) PatchKubeEnvForArch(ctx context.Context, targetArch, gkeVersion string, httpClient *http.Client) error {
	return m.apply(func(values MetadataValues) error {
		return PatchKubeEnvForArch(ctx, values, targetArch, gkeVersion, httpClient)
	})
}

func (m *InstanceMetadata) RenderKubeletConfig(nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, capacityType string) error {
	return m.apply(func(values MetadataValues) error {
		return RenderKubeletConfigMetadata(values, nodeClass, instanceType, capacityType)
	})
}

func (m *InstanceMetadata) AppendSecondaryBootDisks(projectID string, nodeClass *v1alpha1.GCENodeClass) {
	_ = m.apply(func(values MetadataValues) error {
		AppendSecondaryBootDisks(projectID, nodeClass, values)
		return nil
	})
}

func (m *InstanceMetadata) apply(patch func(MetadataValues) error) error {
	values := m.values()
	if err := patch(values); err != nil {
		return err
	}
	m.setFromValues(values)
	return nil
}

func (m *InstanceMetadata) setFromValues(values MetadataValues) {
	m.KubeEnv = KubeEnv{value: values[KubeEnvKey]}
	m.KubeLabels = ParseKubeLabels(values[KubeLabelsKey])
	m.KubeletConfig = KubeletConfig{value: values[KubeletConfigKey]}
	m.CustomMetadata = MetadataValues{}
	for key, value := range values {
		switch key {
		case KubeEnvKey, KubeLabelsKey, KubeletConfigKey:
			continue
		default:
			m.CustomMetadata[key] = value
		}
	}
}

func (m *InstanceMetadata) values() MetadataValues {
	values := MetadataValues{}
	for key, value := range m.CustomMetadata {
		values[key] = value
	}
	values[KubeEnvKey] = m.KubeEnv.value
	values[KubeLabelsKey] = m.KubeLabels.String()
	values[KubeletConfigKey] = m.KubeletConfig.value
	return values
}

func FromAPI(api *compute.Metadata) MetadataValues {
	m := MetadataValues{}
	if api == nil {
		return m
	}
	for _, item := range api.Items {
		if item == nil || item.Key == "" {
			continue
		}
		m[item.Key] = lo.FromPtr(item.Value)
	}
	return m
}

func (m MetadataValues) ToAPI() *compute.Metadata {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	items := make([]*compute.MetadataItems, 0, len(keys))
	for _, key := range keys {
		items = append(items, &compute.MetadataItems{Key: key, Value: lo.ToPtr(m[key])})
	}
	return &compute.Metadata{Items: items}
}

func (m MetadataValues) MergeUser(user map[string]string, reserved map[string]struct{}) {
	if m == nil {
		return
	}
	for key, value := range user {
		if value == "" {
			continue
		}
		if _, ok := reserved[key]; ok {
			continue
		}
		m[key] = value
	}
}

func ReplaceAPI(target *compute.Metadata, m MetadataValues) {
	if target == nil {
		return
	}
	target.Items = m.ToAPI().Items
}
