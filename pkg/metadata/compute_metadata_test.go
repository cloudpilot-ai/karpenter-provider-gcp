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
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestFromSourceTemplateDropsSourceLabelsAndTaintsFromTargetState(t *testing.T) {
	model, err := FromSourceTemplate(&compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeLabelsKey, Value: lo.ToPtr("source-kube-label=true")},
		{Key: KubeEnvKey, Value: lo.ToPtr("OTHER: value\nKUBELET_ARGS: --v=2 --max-pods=110 --node-labels=source-node-label=true --register-with-taints=source=true:NoSchedule\n")},
		{Key: KubeletConfigKey, Value: lo.ToPtr("nodeStatusUpdateFrequency: 10s\n")},
		{Key: "cluster-name", Value: lo.ToPtr("source-cluster")},
		{Key: "unowned-source-key", Value: lo.ToPtr("source")},
	}})
	require.NoError(t, err)

	require.Empty(t, model.kubeLabels)
	require.Empty(t, model.kubeEnv.kubeletArgs.nodeLabels)
	require.Empty(t, model.kubeEnv.kubeletArgs.nodeTaints)
	require.Equal(t, []string{"--v=2"}, model.kubeEnv.kubeletArgs.other)
	require.Equal(t, "10s", model.kubeletConfig.NodeStatusUpdateFrequency.Duration.String())
	require.Empty(t, model.customMetadata)

	rendered, err := model.ToComputeMetadata()
	require.NoError(t, err)
	values := metadataValuesFromCompute(rendered)
	require.Equal(t, "source-cluster", values["cluster-name"])
	require.NotContains(t, values, "unowned-source-key")
	require.NotContains(t, values[KubeEnvKey], "source-node-label=true")
	require.NotContains(t, values[KubeEnvKey], "source=true:NoSchedule")
}

func TestFromSourceTemplateRetainsExplicitGKEBootstrapMetadata(t *testing.T) {
	source := map[string]string{
		"cluster-location":           "europe-west4-a",
		"cluster-name":               "karpenter-e2e-cluster",
		"cluster-uid":                "uid",
		"common-psm1":                "common",
		"configure-sh":               "configure",
		"containerd-configure-sh":    "containerd",
		"disable-address-manager":    "true",
		"disable-legacy-endpoints":   "true",
		"enable-os-login":            "false",
		"gci-ensure-gke-docker":      "ensure",
		"gci-metrics-enabled":        "true",
		"gci-update-strategy":        "update",
		"google-compute-enable-pcid": "true",
		"install-ssh-psm1":           "ssh",
		"instance-template":          "template",
		"k8s-node-setup-psm1":        "node-setup",
		"kubeconfig":                 "kubeconfig",
		"serial-port-logging-enable": "true",
		"startup-script":             "startup",
		"user-data":                  "user-data",
		"user-profile-psm1":          "profile",
		"windows-startup-script-ps1": "windows-startup",
		KubeLabelsKey:                "source=true",
		"unknown-source-key":         "drop-me",
	}
	items := make([]*compute.MetadataItems, 0, len(source))
	for key, value := range source {
		items = append(items, &compute.MetadataItems{Key: key, Value: lo.ToPtr(value)})
	}
	model, err := FromSourceTemplate(&compute.Metadata{Items: items})
	require.NoError(t, err)
	model.SetCustomMetadata(CustomMetadata{"serial-port-logging-enable": "false"})

	rendered, err := model.ToComputeMetadata()
	require.NoError(t, err)
	values := metadataValuesFromCompute(rendered)
	for key, value := range source {
		if key == KubeLabelsKey || key == "unknown-source-key" || key == "serial-port-logging-enable" {
			continue
		}
		require.Equal(t, value, values[key], key)
	}
	require.Equal(t, "false", values["serial-port-logging-enable"])
	require.NotEqual(t, "source=true", values[KubeLabelsKey])
	require.NotContains(t, values, "unknown-source-key")
}

func TestFromSourceTemplatePreservesUnknownKubeEnvEntries(t *testing.T) {
	model, err := FromSourceTemplate(&compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeEnvKey, Value: lo.ToPtr("OTHER: value\ncustom-key: custom-value\nKUBELET_ARGS: --v=2\n")},
	}})
	require.NoError(t, err)

	rendered, err := model.ToComputeMetadata()
	require.NoError(t, err)
	kubeEnv := metadataValuesFromCompute(rendered)[KubeEnvKey]
	require.Contains(t, kubeEnv, "OTHER: value")
	require.Contains(t, kubeEnv, "custom-key: custom-value")
	require.Contains(t, kubeEnv, "KUBELET_ARGS: --v=2")
}

func TestFromSourceTemplatePreservesRawGKEKubeEnvValues(t *testing.T) {
	raw := strings.Join([]string{
		`NODE_PROBLEM_DETECTOR_ADC_CONFIG: "\n{\n\t\"type\": \"external_account\",\n\t\"subject_token_type\":`,
		`  \"node_service_account_id_token\"\n}"`,
		`RENDERED_INSTALLABLES: {"component":{"containerArgs":["bash","-uexc","[[ -f /host/bin ]] && exit 0\n/install.sh\n"]}}`,
		`KUBELET_ARGS: --v=2 --node-labels=source=true --register-with-taints=source=true:NoSchedule`,
	}, "\n")
	model, err := FromSourceTemplate(&compute.Metadata{Items: []*compute.MetadataItems{{Key: KubeEnvKey, Value: lo.ToPtr(raw)}}})
	require.NoError(t, err)
	model.SetKubeletMaxPods(110)

	rendered, err := model.ToComputeMetadata()
	require.NoError(t, err)
	kubeEnv := metadataValuesFromCompute(rendered)[KubeEnvKey]
	require.Contains(t, kubeEnv, `NODE_PROBLEM_DETECTOR_ADC_CONFIG: "\n{\n\t\"type\": \"external_account\",\n\t\"subject_token_type\":`)
	require.Contains(t, kubeEnv, `  \"node_service_account_id_token\"\n}"`)
	require.Contains(t, kubeEnv, `RENDERED_INSTALLABLES: {"component":{"containerArgs":["bash","-uexc","[[ -f /host/bin ]] && exit 0\n/install.sh\n"]}}`)
	require.NotContains(t, kubeEnv, "source=true")
}

func TestFromSourceTemplateRoundTripsUnownedKubeEnvEntries(t *testing.T) {
	model, err := FromSourceTemplate(&compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeEnvKey, Value: lo.ToPtr(strings.Join([]string{
			"SIMPLE: value",
			"MULTILINE: first line",
			"  second line",
			"  third line",
			"KUBELET_ARGS: --v=2 --cloud-provider=external --max-pods=110 --node-labels=source=true --register-with-taints=source=true:NoSchedule",
			"  --container-runtime-endpoint=unix:///run/containerd/containerd.sock",
			"ZONE: europe-west4-a",
		}, "\n"))},
	}})
	require.NoError(t, err)

	rendered, err := model.ToComputeMetadata()
	require.NoError(t, err)
	kubeEnv := metadataValuesFromCompute(rendered)[KubeEnvKey]
	require.Contains(t, kubeEnv, "SIMPLE: value")
	require.Contains(t, kubeEnv, "MULTILINE: first line\n  second line\n  third line")
	require.Contains(t, kubeEnv, "ZONE: europe-west4-a")
	require.Contains(t, kubeEnv, "--v=2")
	require.Contains(t, kubeEnv, "--cloud-provider=external")
	require.Contains(t, kubeEnv, "--container-runtime-endpoint=unix:///run/containerd/containerd.sock")
	require.NotContains(t, kubeEnv, "--max-pods=110")
	require.NotContains(t, kubeEnv, "--node-labels=source=true")
	require.NotContains(t, kubeEnv, "--register-with-taints=source=true:NoSchedule")
}

func TestToComputeMetadataRendersTypedTargetState(t *testing.T) {
	model, err := FromSourceTemplate(&compute.Metadata{Items: []*compute.MetadataItems{
		{Key: KubeEnvKey, Value: lo.ToPtr("OTHER: value\nKUBELET_ARGS: --v=2 --node-labels=source=true --register-with-taints=source=true:NoSchedule\n")},
	}})
	require.NoError(t, err)

	model.SetKubeLabels(Labels{"kube": "label"})
	model.SetRegistrationNodeLabels(Labels{"node": "label"})
	model.SetKubeletMaxPods(32)
	model.SetNodeTaints(Taints{{Key: "target", Value: "true", Effect: corev1.TaintEffectNoSchedule}})
	model.SetCustomMetadata(CustomMetadata{"custom": "value"})

	rendered, err := model.ToComputeMetadata()
	require.NoError(t, err)
	values := metadataValuesFromCompute(rendered)

	require.Equal(t, "kube=label", values[KubeLabelsKey])
	require.Equal(t, "value", values["custom"])
	require.Contains(t, values[KubeEnvKey], "OTHER: value")
	require.Contains(t, values[KubeEnvKey], "--v=2")
	require.Contains(t, values[KubeEnvKey], "--max-pods=32")
	require.Contains(t, values[KubeEnvKey], "--node-labels=node=label")
	require.Contains(t, values[KubeEnvKey], "--register-with-taints=target=true:NoSchedule")
	require.NotContains(t, values[KubeEnvKey], "source=true")
}

func TestToComputeInstanceLabelsReturnsGCPMapShapeCopy(t *testing.T) {
	model := NewInstanceMetadata()
	model.SetInstanceLabels(InstanceLabels{"a": "1"})

	labels := model.ToComputeInstanceLabels()
	labels["a"] = "mutated"

	require.Equal(t, map[string]string{"a": "1"}, model.ToComputeInstanceLabels())
}

func TestLabelCompositionHelpers(t *testing.T) {
	model := NewInstanceMetadata()

	model.SetKubeLabel("kube", "old")
	model.SetKubeLabel("kube", "new")
	model.SetRegistrationNodeLabel("node", "old")
	model.SetRegistrationNodeLabel("node", "new")

	require.Equal(t, Labels{"kube": "new"}, model.kubeLabels)
	require.Equal(t, Labels{"node": "new"}, model.kubeEnv.kubeletArgs.nodeLabels)
}

func TestSettersCopyInputMaps(t *testing.T) {
	model := NewInstanceMetadata()
	kubeLabels := Labels{"k": "v"}
	registrationLabels := Labels{"n": "v"}
	instanceLabels := InstanceLabels{"i": "v"}
	customMetadata := CustomMetadata{"c": "v"}

	model.SetKubeLabels(kubeLabels)
	model.SetRegistrationNodeLabels(registrationLabels)
	model.SetInstanceLabels(instanceLabels)
	model.SetCustomMetadata(customMetadata)

	kubeLabels["k"] = "mutated"
	registrationLabels["n"] = "mutated"
	instanceLabels["i"] = "mutated"
	customMetadata["c"] = "mutated"

	require.Equal(t, Labels{"k": "v"}, model.kubeLabels)
	require.Equal(t, Labels{"n": "v"}, model.kubeEnv.kubeletArgs.nodeLabels)
	require.Equal(t, InstanceLabels{"i": "v"}, model.instanceLabels)
	require.Equal(t, CustomMetadata{"c": "v"}, model.customMetadata)
}
