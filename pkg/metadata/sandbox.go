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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

const sandboxTaint = v1alpha1.LabelSandboxRuntime + "=" + v1alpha1.SandboxRuntimeGVISOR + ":NoSchedule"

// SandboxConfig contains only target-owned native runtime settings. A nil
// configuration removes inherited enabling settings without changing credentials.
type SandboxConfig struct {
	Version, Digest string
	CoreTags        bool
}

var sandboxEnablingFields = []string{
	"CONTAINERD_SANDBOX_POD_ANNOTATIONS", "CONTAINERD_SANDBOX_RUNTIME_ENGINE", "CONTAINERD_SANDBOX_RUNTIME_HANDLER", "CONTAINERD_SANDBOX_RUNTIME_ROOT", "CONTAINERD_SANDBOX_RUNTIME_TYPE",
	"GVISOR_CONTAINERD_SHIM_PATH", "GVISOR_CONTAINERD_SHIM_CONFIG_PATH", "GVISOR_CORE_TAGS", "GVISOR_ENABLE_SMT", "GVISOR_PLATFORM", "GVISOR_REPRODUCE_NFTABLES", "GVISOR_INSTALLER_IMAGE_HASH", "GVISOR_NET_RAW", "GVISOR_SECCOMP",
}

// DecodeKubeEnvScalar accepts exactly one YAML string, not a mapping, implicit
// boolean, sequence, extra document or trailing unparsed tokens.
func DecodeKubeEnvScalar(raw string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return "", fmt.Errorf("expected one YAML string scalar")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("expected one YAML string scalar")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.ScalarNode || document.Content[0].Tag != "!!str" {
		return "", fmt.Errorf("expected one YAML string scalar")
	}
	return document.Content[0].Value, nil
}

// ApplySandbox owns runtime enabling flags, just the gvisor installable, and
// runtime label/taint copies. Other installables, metadata and auth are untouched.
func (m *InstanceMetadata) ApplySandbox(config *SandboxConfig) error {
	if err := m.mergeSandboxInstallable(config); err != nil {
		return err
	}
	if err := m.patchAutoscalerSandbox(config != nil); err != nil {
		return err
	}
	for _, key := range sandboxEnablingFields {
		m.UnsetKubeEnvEntry(key)
	}
	delete(m.kubeLabels, v1alpha1.LabelSandboxRuntime)
	delete(m.kubeEnv.kubeletArgs.nodeLabels, v1alpha1.LabelSandboxRuntime)
	if err := m.patchSandboxNodeTaints(config != nil); err != nil {
		return err
	}
	if config == nil {
		return nil
	}
	for key, value := range map[string]string{
		"CONTAINERD_SANDBOX_POD_ANNOTATIONS": `"dev.gvisor.*", "gke-pod-snapshot-*"`,
		"CONTAINERD_SANDBOX_RUNTIME_ENGINE":  "/home/containerd/usr/local/sbin/runsc",
		"CONTAINERD_SANDBOX_RUNTIME_HANDLER": "gvisor", "CONTAINERD_SANDBOX_RUNTIME_ROOT": "/run/containerd/runsc", "CONTAINERD_SANDBOX_RUNTIME_TYPE": "io.containerd.runsc.v1",
		"GVISOR_CONTAINERD_SHIM_PATH": "/home/containerd/usr/local/bin/containerd-shim-runsc-v1",
		"GVISOR_ENABLE_SMT":           "true", "GVISOR_PLATFORM": "xemu", "GVISOR_REPRODUCE_NFTABLES": "true",
	} {
		m.SetKubeEnvEntry(key, strconv.Quote(value))
	}
	if config.CoreTags {
		m.SetKubeEnvEntry("GVISOR_CORE_TAGS", `"true"`)
	}
	m.SetKubeLabel(v1alpha1.LabelSandboxRuntime, v1alpha1.SandboxRuntimeGVISOR)
	m.SetRegistrationNodeLabel(v1alpha1.LabelSandboxRuntime, v1alpha1.SandboxRuntimeGVISOR)

	return nil
}

func (m *InstanceMetadata) mergeSandboxInstallable(config *SandboxConfig) error {
	raw, exists := m.GetKubeEnvEntry("RENDERED_INSTALLABLES")
	components := map[string]json.RawMessage{}
	if exists {
		var err error
		components, err = decodeInstallableComponents(raw)
		if err != nil {
			return err
		}
	}
	entries, err := gvisorInstallables(components)
	if err != nil {
		return err
	}
	delete(entries, "gvisor")
	if config != nil {
		b, err := nativeSandboxInstallable(config)
		if err != nil {
			return err
		}
		entries["gvisor"] = b
	}
	delete(components, "gvisor")
	if len(entries) > 0 {
		b, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		components["gvisor"] = b
	}
	if !exists && config == nil {
		return nil
	}
	b, err := json.Marshal(components)
	if err != nil {
		return err
	}
	m.SetKubeEnvEntry("RENDERED_INSTALLABLES", strconv.Quote(string(b)))
	return nil
}

func patchRuntimeList(value string, taints, enabled bool) string {
	var result []string
	for _, entry := range strings.Split(value, ",") {
		if entry == "" {
			continue
		}
		key, _, _ := strings.Cut(entry, "=")
		key, _, _ = strings.Cut(key, ":")
		if key == v1alpha1.LabelSandboxRuntime && (!taints || strings.HasSuffix(entry, ":NoSchedule")) {
			continue
		}
		result = append(result, entry)
	}
	if enabled {
		entry := v1alpha1.LabelSandboxRuntime + "=gvisor"
		if taints {
			entry = sandboxTaint
		}
		result = append(result, entry)
	}
	return strings.Join(result, ",")
}

func (m *InstanceMetadata) patchAutoscalerSandbox(enabled bool) error {
	raw, exists := m.GetKubeEnvEntry("AUTOSCALER_ENV_VARS")
	if !exists {
		return nil
	}
	value, err := DecodeKubeEnvScalar(raw)
	if err != nil {
		return fmt.Errorf("AUTOSCALER_ENV_VARS: %w", err)
	}
	fields, err := parseAutoscalerAssignments(value)
	if err != nil {
		return err
	}
	var assignments []string
	seen := map[string]bool{}
	for _, field := range fields {
		key, val := field[0], field[1]
		seen[key] = true
		assignment, err := patchAutoscalerAssignment(key, val, enabled)
		if err != nil {
			return err
		}
		if assignment != "" {
			assignments = append(assignments, assignment)
		}
	}
	if enabled {
		if !seen["node_labels"] {
			assignments = append(assignments, "node_labels="+v1alpha1.LabelSandboxRuntime+"=gvisor")
		}
		if !seen["node_taints"] {
			assignments = append(assignments, "node_taints="+sandboxTaint)
		}
	}
	m.SetKubeEnvEntry("AUTOSCALER_ENV_VARS", strconv.Quote(strings.Join(assignments, ";")))
	return nil
}

func parseAutoscalerAssignments(value string) ([][2]string, error) {
	var fields [][2]string
	seen := map[string]bool{}
	for _, assignment := range strings.Split(value, ";") {
		key, val, ok := strings.Cut(assignment, "=")
		if !ok || key == "" || strings.TrimSpace(key) != key || seen[key] {
			return nil, fmt.Errorf("AUTOSCALER_ENV_VARS contains malformed or duplicate assignments")
		}
		seen[key] = true
		fields = append(fields, [2]string{key, val})
	}
	return fields, nil
}

func patchAutoscalerAssignment(key, val string, enabled bool) (string, error) {
	if key == "node_labels" || key == "node_taints" {
		if err := validateRuntimeList(val, key == "node_taints"); err != nil {
			return "", fmt.Errorf("AUTOSCALER_ENV_VARS.%s: %w", key, err)
		}
		val = patchRuntimeList(val, key == "node_taints", enabled)
		if key == "node_taints" && val == "" {
			return "", nil
		}
	}
	return key + "=" + val, nil
}

const (
	SandboxRuntimeMetadataKey   = "karpenter-gcp-sandbox-runtime"
	SandboxInstallerMetadataKey = "karpenter-gcp-sandbox-installer"
	SandboxProfileMetadataKey   = "karpenter-gcp-sandbox-profile"
	SandboxRuntimeInstanceLabel = "karpenter-gcp-sandbox-runtime"
	SandboxClassUIDMetadataKey  = "karpenter-gcp-sandbox-class-uid"
	SandboxClassHashMetadataKey = "karpenter-gcp-sandbox-class-hash"
	SandboxPoolUIDMetadataKey   = "karpenter-gcp-sandbox-pool-uid"
)

// SandboxIdentity is durable VM-factual identity. Full immutable digests belong
// in metadata, not GCE labels (which are limited to 63 characters).
type SandboxIdentity struct {
	Runtime   string `json:"runtime"`
	Installer string `json:"installer"`
	Profile   string `json:"profile"`
	// Launch bindings are distinct from runtime capability and are never inferred on reads.
	ClassUID  string `json:"classUID"`
	ClassHash string `json:"classHash"`
	PoolUID   string `json:"poolUID"`
	// Malformed retains owned-key presence/values even for empty or duplicate entries.
	Malformed bool `json:"malformed,omitempty"`
}

func (m *InstanceMetadata) SetSandboxIdentity(identity SandboxIdentity) {
	m.sourceMetadata[SandboxRuntimeMetadataKey] = identity.Runtime
	m.sourceMetadata[SandboxInstallerMetadataKey] = identity.Installer
	m.sourceMetadata[SandboxProfileMetadataKey] = identity.Profile
	m.sourceMetadata[SandboxClassUIDMetadataKey] = identity.ClassUID
	m.sourceMetadata[SandboxClassHashMetadataKey] = identity.ClassHash
	if identity.PoolUID != "" {
		m.sourceMetadata[SandboxPoolUIDMetadataKey] = identity.PoolUID
	}
	m.instanceLabels[SandboxRuntimeInstanceLabel] = identity.Runtime
}

func validateRuntimeList(value string, taints bool) error {
	if value == "" {
		return nil
	}
	seen := map[string]bool{}
	for _, entry := range strings.Split(value, ",") {
		identity, err := runtimeListIdentity(entry, taints)
		if err != nil {
			return err
		}
		if seen[identity] {
			return fmt.Errorf("duplicate runtime label/taint entries")
		}
		seen[identity] = true
	}
	return nil
}

func runtimeListIdentity(entry string, taints bool) (string, error) {
	token, effect := entry, ""
	if taints {
		var ok bool
		token, effect, ok = strings.Cut(token, ":")
		if !ok || (effect != "NoSchedule" && effect != "PreferNoSchedule" && effect != "NoExecute") {
			return "", fmt.Errorf("invalid taint effect")
		}
	}
	key, _, ok := strings.Cut(token, "=")
	if key == "" || strings.TrimSpace(key) != key || (!taints && !ok) {
		return "", fmt.Errorf("malformed runtime label/taint entries")
	}
	return key + ":" + effect, nil
}

func (m *InstanceMetadata) patchSandboxNodeTaints(enabled bool) error {
	m.kubeEnv.kubeletArgs.nodeTaints = slices.DeleteFunc(m.kubeEnv.kubeletArgs.nodeTaints, func(t corev1.Taint) bool {
		return t.Key == v1alpha1.LabelSandboxRuntime && t.Effect == corev1.TaintEffectNoSchedule
	})
	raw, exists := m.GetKubeEnvEntry("NODE_TAINTS")
	taints := ""
	if exists {
		var err error
		taints, err = DecodeKubeEnvScalar(raw)
		if err != nil {
			return fmt.Errorf("NODE_TAINTS: %w", err)
		}
	}
	if err := validateRuntimeList(taints, true); err != nil {
		return fmt.Errorf("NODE_TAINTS: %w", err)
	}
	taints = patchRuntimeList(taints, true, enabled)
	if exists || enabled {
		m.SetKubeEnvEntry("NODE_TAINTS", strconv.Quote(taints))
	}
	if enabled {
		m.AppendNodeTaint(corev1.Taint{Key: v1alpha1.LabelSandboxRuntime, Value: "gvisor", Effect: corev1.TaintEffectNoSchedule})
	}
	return nil
}

func nativeSandboxInstallable(config *SandboxConfig) (json.RawMessage, error) {
	// installable.py appends @digestAlgo:digest to remoteURL itself.
	if len(config.Digest) != 71 || !strings.HasPrefix(config.Digest, "sha256:") || config.Version == "" {
		return nil, fmt.Errorf("invalid sandbox installer identity")
	}
	digest := strings.TrimPrefix(config.Digest, "sha256:")
	for _, c := range digest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return nil, fmt.Errorf("invalid sandbox installer digest")
		}
	}
	installer := map[string]any{
		"kind": "container", "apiVersion": "installable.gke.io/v1", "metadata": map[string]any{"name": "gvisor", "creationTimestamp": nil},
		"os": "linux", "arch": "MULTI", "version": config.Version, "remoteURL": "gke.gcr.io/gke-gvisor-installer:" + config.Version,
		"digest": digest, "digestAlgo": "sha256", "run": map[string]any{"ctrArgs": []string{"--mount=type=bind,src=/,dst=/host,options=rbind", "--privileged"}},
	}
	b, err := json.Marshal(installer)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func decodeInstallableComponents(raw string) (map[string]json.RawMessage, error) {
	components := map[string]json.RawMessage{}
	value := raw
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		var err error
		value, err = DecodeKubeEnvScalar(raw)
		if err != nil {
			return nil, fmt.Errorf("RENDERED_INSTALLABLES: malformed JSON scalar")
		}
	}
	if err := json.Unmarshal([]byte(value), &components); err != nil || components == nil {
		return nil, fmt.Errorf("RENDERED_INSTALLABLES must be a JSON object")
	}
	return components, nil
}

func gvisorInstallables(components map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	entries := map[string]json.RawMessage{}
	if raw, ok := components["gvisor"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
			return nil, fmt.Errorf("RENDERED_INSTALLABLES.gvisor must be a JSON object")
		}
	}
	return entries, nil
}

// SetSandboxPoolUID binds only a verified upstream owner, never a label.
func (m *InstanceMetadata) SetSandboxPoolUID(uid string) {
	if uid != "" {
		m.sourceMetadata[SandboxPoolUIDMetadataKey] = uid
	}
}
