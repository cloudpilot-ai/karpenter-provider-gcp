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

import (
	"maps"
	"sort"
	"strings"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
	"sigs.k8s.io/yaml"
)

// retainedSourceMetadataKeys are raw source-template keys copied through from
// GKE-managed instance templates. kube-env and kubelet-config are parsed
// separately because Karpenter owns selected fields inside those values.
var retainedSourceMetadataKeys = []string{
	"cluster-location",
	"cluster-name",
	"cluster-uid",
	"common-psm1",
	"configure-sh",
	"containerd-configure-sh",
	"disable-address-manager",
	"disable-legacy-endpoints",
	"enable-os-login",
	"gci-ensure-gke-docker",
	"gci-metrics-enabled",
	"gci-update-strategy",
	"google-compute-enable-pcid",
	"install-ssh-psm1",
	"instance-template",
	"k8s-node-setup-psm1",
	"kubeconfig",
	"serial-port-logging-enable",
	"startup-script",
	"user-data",
	"user-profile-psm1",
	"windows-startup-script-ps1",
}

// NewInstanceMetadata creates an empty target metadata model.
func NewInstanceMetadata() *InstanceMetadata {
	return &InstanceMetadata{
		sourceMetadata: CustomMetadata{},
		kubeLabels:     Labels{},
		customMetadata: CustomMetadata{},
		instanceLabels: InstanceLabels{},
	}
}

// FromSourceTemplate initializes the typed model from the source template fields
// that remain inherited. Bootstrap labels and taints are target-owned after
// #436 and are not populated from source kube-labels or KUBELET_ARGS.
func FromSourceTemplate(source *compute.Metadata) (*InstanceMetadata, error) {
	model := NewInstanceMetadata()
	values := metadataValuesFromCompute(source)
	for _, key := range retainedSourceMetadataKeys {
		if value, ok := values[key]; ok {
			model.sourceMetadata[key] = value
		}
	}
	model.kubeEnv = parseKubeEnv(values[KubeEnvKey])
	if raw := values[KubeletConfigKey]; raw != "" {
		if err := yaml.Unmarshal([]byte(raw), &model.kubeletConfig); err != nil {
			return nil, err
		}
	}
	return model, nil
}

func (m *InstanceMetadata) SetKubeLabels(labels Labels) {
	if m == nil {
		return
	}
	m.kubeLabels = maps.Clone(labels)
}

func (m *InstanceMetadata) SetKubeLabel(key, value string) {
	if m == nil || key == "" {
		return
	}
	if m.kubeLabels == nil {
		m.kubeLabels = Labels{}
	}
	m.kubeLabels[key] = value
}

func (m *InstanceMetadata) SetInstanceLabels(labels InstanceLabels) {
	if m == nil {
		return
	}
	m.instanceLabels = maps.Clone(labels)
}

func (m *InstanceMetadata) GetKubeEnvEntry(key string) (string, bool) {
	if m == nil || key == "" {
		return "", false
	}
	value, ok := m.kubeEnv.entries[key]
	return value, ok
}

func (m *InstanceMetadata) SetKubeEnvEntry(key, value string) {
	if m == nil || key == "" {
		return
	}
	if m.kubeEnv.entries == nil {
		m.kubeEnv.entries = KubeEnvEntries{}
	}
	m.kubeEnv.entries[key] = value
}

func (m *InstanceMetadata) UnsetKubeEnvEntry(key string) {
	if m == nil || key == "" {
		return
	}
	delete(m.kubeEnv.entries, key)
}

func (m *InstanceMetadata) UpdateKubeletConfig(update func(*kubeletconfig.KubeletConfiguration)) {
	if m == nil || update == nil {
		return
	}
	update(&m.kubeletConfig)
}

func (m *InstanceMetadata) SetCustomMetadata(values CustomMetadata) {
	if m == nil {
		return
	}
	m.customMetadata = CustomMetadata{}
	for key, value := range values {
		if key == "" {
			continue
		}
		m.customMetadata[key] = value
	}
}

// ToComputeMetadata renders the typed model to Compute Engine metadata shape.
func (m *InstanceMetadata) ToComputeMetadata() (*compute.Metadata, error) {
	if m == nil {
		return toComputeMetadata(metadataValues{}), nil
	}
	values := metadataValues{}
	for key, value := range m.sourceMetadata {
		values[key] = value
	}
	for key, value := range m.customMetadata {
		values[key] = value
	}
	values[KubeEnvKey] = m.kubeEnv.String()
	values[KubeLabelsKey] = m.kubeLabels.String()
	rawKubeletConfig, err := yaml.Marshal(m.kubeletConfig)
	if err != nil {
		return nil, err
	}
	if string(rawKubeletConfig) != "{}\n" {
		values[KubeletConfigKey] = string(rawKubeletConfig)
	}
	return toComputeMetadata(values), nil
}

// ToComputeInstanceLabels converts instance labels to the GCP API map shape.
func (m *InstanceMetadata) ToComputeInstanceLabels() map[string]string {
	labels := map[string]string{}
	if m == nil {
		return labels
	}
	for key, value := range m.instanceLabels {
		labels[key] = value
	}
	return labels
}

type metadataValues map[string]string

func metadataValuesFromCompute(api *compute.Metadata) metadataValues {
	values := metadataValues{}
	if api == nil {
		return values
	}
	for _, item := range api.Items {
		if item == nil || item.Key == "" {
			continue
		}
		values[item.Key] = lo.FromPtr(item.Value)
	}
	return values
}

func toComputeMetadata(values metadataValues) *compute.Metadata {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	items := make([]*compute.MetadataItems, 0, len(keys))
	for _, key := range keys {
		items = append(items, &compute.MetadataItems{Key: key, Value: lo.ToPtr(values[key])})
	}
	return &compute.Metadata{Items: items}
}

func parseKubeEnv(raw string) KubeEnv {
	env := KubeEnv{entries: KubeEnvEntries{}}
	var currentKey string
	for _, line := range strings.Split(raw, "\n") {
		if key, value, ok := splitKubeEnvEntry(line); ok {
			currentKey = key
			env.entries[key] = value
			continue
		}
		if currentKey != "" {
			env.entries[currentKey] += "\n" + line
		}
	}
	if rawArgs := env.entries["KUBELET_ARGS"]; rawArgs != "" {
		env.kubeletArgs = ParseSourceKubeletArgs(rawArgs)
		delete(env.entries, "KUBELET_ARGS")
	}
	return env
}

func splitKubeEnvEntry(line string) (string, string, bool) {
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return "", "", false
	}
	key, value, ok := strings.Cut(line, ": ")
	if !ok || key == "" {
		return "", "", false
	}
	return key, value, true
}

func (e KubeEnv) String() string {
	keys := make([]string, 0, len(e.entries))
	for key := range e.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		lines = append(lines, key+": "+e.entries[key])
	}
	if args := e.kubeletArgs.String(); args != "" {
		lines = append(lines, "KUBELET_ARGS: "+args)
	}
	return strings.Join(lines, "\n")
}
