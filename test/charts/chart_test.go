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

package charts_test

import (
	"bytes"
	"io"
	"os/exec"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/yaml"
)

func render(t *testing.T, template string, args ...string) []byte {
	t.Helper()
	command := append([]string{"template", "karpenter", "../../charts/karpenter", "--show-only", template}, args...)
	output, err := exec.Command("helm", command...).CombinedOutput()
	require.NoError(t, err, "%s", output)
	return output
}

func deploymentEnv(t *testing.T, args ...string) map[string]string {
	t.Helper()
	var deployment appsv1.Deployment
	require.NoError(t, yaml.Unmarshal(render(t, "templates/deployment.yaml", args...), &deployment))
	env := map[string]string{}
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	return env
}

func TestLegacyRepairSetting(t *testing.T) {
	require.Equal(t, "false", deploymentEnv(t)["LEGACY_NODE_REPAIR"])
	require.Equal(t, "true", deploymentEnv(t, "--set", "controller.settings.legacyNodeRepair=true")["LEGACY_NODE_REPAIR"])
}

func TestSchedulerConfiguration(t *testing.T) {
	require.NotContains(t, deploymentEnv(t), "SCHEDULER_CONFIG")
	config := `{"podTopologySpread":{"defaultConstraints":[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}]}}`
	env := deploymentEnv(t, "--set-json", "controller.settings.schedulerConfig="+config)
	parsed, err := options.ParseSchedulerConfiguration(env["SCHEDULER_CONFIG"])
	require.NoError(t, err)
	require.NotNil(t, parsed)
	require.Len(t, parsed.PodTopologySpread.DefaultConstraints, 1)
	require.Equal(t, "topology.kubernetes.io/zone", parsed.PodTopologySpread.DefaultConstraints[0].TopologyKey)
}

func TestMalformedSchedulerConfiguration(t *testing.T) {
	config := `{"podTopologySpread":{"defaultConstraints":[{"maxSkew":0,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}]}}`
	env := deploymentEnv(t, "--set-json", "controller.settings.schedulerConfig="+config)
	_, err := options.ParseSchedulerConfiguration(env["SCHEDULER_CONFIG"])
	require.ErrorContains(t, err, "maxSkew must be greater than 0")
}

func TestFeatureGateOverrides(t *testing.T) {
	for _, legacy := range []string{"false", "true"} {
		env := deploymentEnv(t, "--set", "controller.featureGates.terminateFirstDrift=true,controller.featureGates.terminateFirstRepair=true,controller.featureGates.podDeletionCostManagement=true,controller.featureGates.nodeRepair=true,controller.settings.legacyNodeRepair="+legacy)
		gates, err := options.ParseFeatureGates(env["FEATURE_GATES"])
		require.NoError(t, err)
		require.True(t, gates.TerminateFirstDrift)
		require.True(t, gates.TerminateFirstRepair)
		require.True(t, gates.PodDeletionCostManagement)
		require.True(t, gates.NodeRepair)
		require.Equal(t, legacy, env["LEGACY_NODE_REPAIR"])
	}
}

func TestOptionalControllerPermissions(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		args := []string{}
		if enabled {
			args = []string{"--set", "controller.featureGates.podDeletionCostManagement=true"}
		}
		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(render(t, "templates/clusterrole-core.yaml", args...)), 4096)
		var rules []rbacv1.PolicyRule
		for {
			var role rbacv1.ClusterRole
			err := decoder.Decode(&role)
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if role.Kind == "ClusterRole" {
				rules = role.Rules
			}
		}
		require.NotEmpty(t, rules)
		allows := func(resource, verb string) bool {
			for _, rule := range rules {
				if slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) {
					return true
				}
			}
			return false
		}
		require.True(t, allows("services", "list"))
		require.True(t, allows("services", "watch"))
		require.Equal(t, enabled, allows("pods", "patch"))
	}
}

func TestFeatureGateDefaults(t *testing.T) {
	env := deploymentEnv(t)
	for _, gate := range []string{
		"NodeRepair=false", "ReservedCapacity=false", "SpotToSpotConsolidation=true",
		"NodeOverlay=false", "StaticCapacity=false", "CapacityBuffer=false",
		"TerminateFirstDrift=false", "TerminateFirstRepair=false", "PodDeletionCostManagement=false",
	} {
		require.Contains(t, env["FEATURE_GATES"], gate)
	}
	gates, err := options.ParseFeatureGates(env["FEATURE_GATES"])
	require.NoError(t, err)
	require.False(t, gates.NodeRepair)
	require.False(t, gates.ReservedCapacity)
	require.True(t, gates.SpotToSpotConsolidation)
}
