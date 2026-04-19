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

package cloudprovider

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func TestInstanceToNodeClaim_PropagatesClusterLocationLabel(t *testing.T) {
	t.Parallel()

	inst := &instance.Instance{
		Name:      "test-node",
		ProjectID: "my-project",
		Location:  "us-central1-f",
		Labels:    map[string]string{utils.LabelClusterLocationKey: "us-central1-f"},
	}

	nc := (&CloudProvider{}).instanceToNodeClaim(inst, nil)

	require.Equal(t, "us-central1-f", nc.Labels[utils.LabelClusterLocationKey],
		"cluster-location label must be propagated from instance to NodeClaim so the GC controller can inspect it")
}

func TestInstanceToNodeClaim_AbsentClusterLocationLabelNotInvented(t *testing.T) {
	t.Parallel()

	// Legacy instances (created before this label was introduced) must not have
	// the label invented in their synthetic NodeClaim — the GC controller relies
	// on label absence to identify and skip these pre-migration nodes.
	inst := &instance.Instance{
		Name:         "legacy-node",
		ProjectID:    "my-project",
		Location:     "us-central1-f",
		CreationTime: time.Now().Add(-5 * time.Minute),
		Labels:       map[string]string{},
	}

	nc := (&CloudProvider{}).instanceToNodeClaim(inst, nil)

	_, hasLabel := nc.Labels[utils.LabelClusterLocationKey]
	require.False(t, hasLabel,
		"NodeClaim built from a label-less instance must not carry cluster-location; GC skip depends on its absence")
}

func TestRepairPolicies_PositiveTolerationDuration(t *testing.T) {
	t.Parallel()
	for _, p := range (&CloudProvider{}).RepairPolicies() {
		require.Greater(t, p.TolerationDuration, time.Duration(0),
			"TolerationDuration for condition %s must be positive", p.ConditionType)
	}
}

func TestRepairPolicies_ContainsNodeReady(t *testing.T) {
	t.Parallel()
	var hasReadyFalse, hasReadyUnknown bool
	for _, p := range (&CloudProvider{}).RepairPolicies() {
		if p.ConditionType != corev1.NodeReady {
			continue
		}
		hasReadyFalse = hasReadyFalse || p.ConditionStatus == corev1.ConditionFalse
		hasReadyUnknown = hasReadyUnknown || p.ConditionStatus == corev1.ConditionUnknown
	}
	require.True(t, hasReadyFalse, "must have NodeReady=False repair policy")
	require.True(t, hasReadyUnknown, "must have NodeReady=Unknown repair policy")
}

func TestRepairPolicies_ContainsGKENPDConditions(t *testing.T) {
	t.Parallel()
	// GKE runs Node Problem Detector by default. These conditions use True=problem polarity.
	expectedNPD := []string{
		"KernelDeadlock",
		"ReadonlyFilesystem",
		"FrequentKubeletRestart",
		"FrequentContainerdRestart",
	}
	policies := (&CloudProvider{}).RepairPolicies()
	conditionSet := map[corev1.NodeConditionType]cloudprovider.RepairPolicy{}
	for _, p := range policies {
		conditionSet[p.ConditionType] = p
	}
	for _, name := range expectedNPD {
		p, ok := conditionSet[corev1.NodeConditionType(name)]
		require.True(t, ok, "must have repair policy for GKE NPD condition %s", name)
		require.Equal(t, corev1.ConditionTrue, p.ConditionStatus,
			"GKE NPD condition %s uses True=problem polarity", name)
	}
}

func TestRepairPolicies_NPDConditionsRequireTrue(t *testing.T) {
	t.Parallel()
	// NPD conditions use True=problem polarity. A node without NPD running will never have
	// these conditions set (they simply don't exist in node.status.conditions), so the
	// node.health controller will never match them — no false-positive replacements occur
	// when NPD is disabled or absent.
	npdConditions := map[corev1.NodeConditionType]bool{
		"KernelDeadlock":            true,
		"ReadonlyFilesystem":        true,
		"FrequentKubeletRestart":    true,
		"FrequentContainerdRestart": true,
	}
	for _, p := range (&CloudProvider{}).RepairPolicies() {
		if npdConditions[p.ConditionType] {
			require.Equal(t, corev1.ConditionTrue, p.ConditionStatus,
				"NPD condition %s must require ConditionTrue; ConditionFalse would fire on nodes without NPD since absent conditions default to False", p.ConditionType)
		}
	}
}
