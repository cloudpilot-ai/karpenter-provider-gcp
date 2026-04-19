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

func TestRepairPolicies_NPDConditionsPolarity(t *testing.T) {
	t.Parallel()
	// GKE Node Problem Detector conditions use True=problem polarity (opposite of NodeReady).
	// ConditionFalse must never be used here: absent conditions default to False in the
	// Kubernetes API, so a False-triggered policy would fire on every node that lacks NPD.
	npdConditions := map[corev1.NodeConditionType]bool{
		"KernelDeadlock":            true,
		"ReadonlyFilesystem":        true,
		"FrequentKubeletRestart":    true,
		"FrequentContainerdRestart": true,
	}
	for _, p := range (&CloudProvider{}).RepairPolicies() {
		if npdConditions[p.ConditionType] {
			require.Equal(t, corev1.ConditionTrue, p.ConditionStatus,
				"NPD condition %s must use ConditionTrue polarity", p.ConditionType)
		}
	}
}
