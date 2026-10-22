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
	"strconv"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestSandboxOverlayNativeInstallableAndCleanup(t *testing.T) {
	source := &compute.Metadata{Items: []*compute.MetadataItems{{Key: KubeEnvKey, Value: lo.ToPtr(strings.Join([]string{
		`RENDERED_INSTALLABLES: {"other":{"tool":{"remoteURL":"preserved"}},"gvisor":{"gvisor":{"version":"stale"},"unrelated":{"value":1}}}`,
		`GVISOR_PLATFORM: "ptrace"`, `GVISOR_CORE_TAGS: "true"`, `GVISOR_NET_RAW: "true"`,
		`GVISOR_HOST_SETTINGS: enforce`, `GVISOR_METRIC_SERVER: 127.0.0.1:9115`,
		`NODE_PROBLEM_DETECTOR_ADC_CONFIG: {"audience":"fixture-auth-unchanged"}`,
		`TPM_BOOTSTRAP_KEY: fixture-key-unchanged`,
		`NODE_TAINTS: "other=present:NoSchedule,sandbox.gke.io/runtime=old:NoSchedule"`,
		`AUTOSCALER_ENV_VARS: "kube_reserved=cpu=100m,memory=100Mi;node_labels=example.com/role=worker,sandbox.gke.io/runtime=old;arch=amd64;os=linux;os_distribution=cos;evictionHard=memory.available=100Mi"`,
	}, "\n"))}}}
	target, err := FromSourceTemplate(source)
	require.NoError(t, err)
	target.SetKubeLabel("example.com/role", "worker")
	target.SetRegistrationNodeLabel("example.com/role", "worker")
	target.SetNodeTaints(Taints{{Key: "other", Value: "present", Effect: corev1.TaintEffectNoSchedule}})
	config := &SandboxConfig{Version: "20260323.0_RC03", Digest: "sha256:" + strings.Repeat("a", 64), CoreTags: true}
	require.NoError(t, target.ApplySandbox(config))
	require.NoError(t, target.ApplySandbox(config)) // no duplicate taints/installables
	values := sandboxRendered(t, target)
	env := values[KubeEnvKey]
	for _, value := range []string{`GVISOR_PLATFORM: "xemu"`, `GVISOR_ENABLE_SMT: "true"`, `GVISOR_REPRODUCE_NFTABLES: "true"`, `GVISOR_CORE_TAGS: "true"`,
		`CONTAINERD_SANDBOX_RUNTIME_HANDLER: "gvisor"`, `CONTAINERD_SANDBOX_RUNTIME_TYPE: "io.containerd.runsc.v1"`,
		`CONTAINERD_SANDBOX_RUNTIME_ENGINE: "/home/containerd/usr/local/sbin/runsc"`, `CONTAINERD_SANDBOX_RUNTIME_ROOT: "/run/containerd/runsc"`,
		`GVISOR_CONTAINERD_SHIM_PATH: "/home/containerd/usr/local/bin/containerd-shim-runsc-v1"`,
	} {
		require.Contains(t, env, value)
	}
	annotations, _ := target.GetKubeEnvEntry("CONTAINERD_SANDBOX_POD_ANNOTATIONS")
	var annotationValue string
	require.NoError(t, yaml.Unmarshal([]byte(annotations), &annotationValue))
	require.Equal(t, `"dev.gvisor.*", "gke-pod-snapshot-*"`, annotationValue)
	require.Contains(t, values[KubeLabelsKey], "sandbox.gke.io/runtime=gvisor")
	require.Contains(t, env, "--node-labels=example.com/role=worker,sandbox.gke.io/runtime=gvisor")
	require.Contains(t, env, "--register-with-taints=other=present:NoSchedule,sandbox.gke.io/runtime=gvisor:NoSchedule")
	raw, _ := target.GetKubeEnvEntry("RENDERED_INSTALLABLES")
	var encoded string
	require.NoError(t, yaml.Unmarshal([]byte(raw), &encoded))
	var components map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(encoded), &components))
	require.JSONEq(t, `{"remoteURL":"preserved"}`, string(components["other"]["tool"]))
	require.JSONEq(t, `{"value":1}`, string(components["gvisor"]["unrelated"]))
	var installer map[string]any
	require.NoError(t, json.Unmarshal(components["gvisor"]["gvisor"], &installer))
	require.Equal(t, "container", installer["kind"])
	require.Equal(t, "installable.gke.io/v1", installer["apiVersion"])
	require.Equal(t, "MULTI", installer["arch"])
	require.Equal(t, "linux", installer["os"])
	require.Equal(t, map[string]any{"name": "gvisor", "creationTimestamp": nil}, installer["metadata"])
	require.Equal(t, "20260323.0_RC03", installer["version"])
	require.Equal(t, "gke.gcr.io/gke-gvisor-installer:20260323.0_RC03", installer["remoteURL"])
	require.Equal(t, strings.Repeat("a", 64), installer["digest"])
	require.Equal(t, "sha256", installer["digestAlgo"])
	require.Equal(t, map[string]any{"ctrArgs": []any{"--mount=type=bind,src=/,dst=/host,options=rbind", "--privileged"}}, installer["run"])
	autoscaler, _ := target.GetKubeEnvEntry("AUTOSCALER_ENV_VARS")
	require.Contains(t, autoscaler, "node_labels=example.com/role=worker,sandbox.gke.io/runtime=gvisor")
	require.Contains(t, autoscaler, "node_taints=sandbox.gke.io/runtime=gvisor:NoSchedule")
	require.Contains(t, autoscaler, "kube_reserved=cpu=100m,memory=100Mi")
	require.Contains(t, autoscaler, "evictionHard=memory.available=100Mi")
	config.CoreTags = false
	require.NoError(t, target.ApplySandbox(config))
	_, exists := target.GetKubeEnvEntry("GVISOR_CORE_TAGS")
	require.False(t, exists)
	require.NoError(t, target.ApplySandbox(nil))
	disabled := sandboxRendered(t, target)
	require.NotContains(t, disabled[KubeLabelsKey], v1alpha1.LabelSandboxRuntime)
	require.NotContains(t, disabled[KubeEnvKey], v1alpha1.LabelSandboxRuntime)
	for _, key := range []string{"GVISOR_PLATFORM", "GVISOR_CORE_TAGS", "GVISOR_ENABLE_SMT", "CONTAINERD_SANDBOX_RUNTIME_HANDLER", "GVISOR_NET_RAW"} {
		_, exists := target.GetKubeEnvEntry(key)
		require.False(t, exists, key)
	}
	require.Contains(t, disabled[KubeEnvKey], "GVISOR_HOST_SETTINGS: enforce")
	require.Contains(t, disabled[KubeEnvKey], "GVISOR_METRIC_SERVER: 127.0.0.1:9115")
	require.Contains(t, disabled[KubeEnvKey], `NODE_PROBLEM_DETECTOR_ADC_CONFIG: {"audience":"fixture-auth-unchanged"}`)
	require.Contains(t, disabled[KubeEnvKey], "TPM_BOOTSTRAP_KEY: fixture-key-unchanged")
	require.Contains(t, disabled[KubeEnvKey], "other=present:NoSchedule")
}

func TestSandboxOverlayRejectsMalformedOwnedStructures(t *testing.T) {
	for _, raw := range []string{`{bad`, `null`, `[]`, `{"gvisor":null}`, `{"gvisor":[]}`, `"not JSON"`} {
		target := NewInstanceMetadata()
		target.SetKubeEnvEntry("RENDERED_INSTALLABLES", raw)
		require.ErrorContains(t, target.ApplySandbox(nil), "RENDERED_INSTALLABLES")
	}
	for _, raw := range []string{"node_labels=a;node_labels=b", "missing-separator", "node_labels=bad", "node_taints=key=value:WrongEffect", "arch=amd64;;os=linux", "[a,b]"} {
		target := NewInstanceMetadata()
		target.SetKubeEnvEntry("AUTOSCALER_ENV_VARS", strconv.Quote(raw))
		require.ErrorContains(t, target.ApplySandbox(nil), "AUTOSCALER_ENV_VARS")
	}
	target := NewInstanceMetadata()
	require.NoError(t, target.ApplySandbox(&SandboxConfig{Version: "20260323.0_RC03", Digest: "sha256:" + strings.Repeat("a", 64)}))
	_, exists := target.GetKubeEnvEntry("AUTOSCALER_ENV_VARS")
	require.False(t, exists)
}

func TestSandboxOverlayAutoscalerCleanupPreservesUnrelatedTaints(t *testing.T) {
	target := NewInstanceMetadata()
	target.SetKubeEnvEntry("AUTOSCALER_ENV_VARS", `'node_taints=dedicated=worker:NoSchedule,sandbox.gke.io/runtime=gvisor:NoSchedule;node_labels=example.com/role=worker,sandbox.gke.io/runtime=gvisor;arch=amd64'`)
	require.NoError(t, target.ApplySandbox(nil))
	raw, _ := target.GetKubeEnvEntry("AUTOSCALER_ENV_VARS")
	require.Equal(t, `"node_taints=dedicated=worker:NoSchedule;node_labels=example.com/role=worker;arch=amd64"`, raw)
}

func sandboxRendered(t *testing.T, target *InstanceMetadata) map[string]string {
	t.Helper()
	rendered, err := target.ToComputeMetadata()
	require.NoError(t, err)
	values := map[string]string{}
	for _, item := range rendered.Items {
		values[item.Key] = *item.Value
	}
	return values
}
