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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
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

func TestDelete_ReturnsNodeClaimNotFoundWhenProviderIDEmpty(t *testing.T) {
	t.Parallel()

	// Without the guard in Delete, the call reaches parseGCEProviderID("") which errors,
	// causing karpenter to retry termination forever so the finalizer never clears.
	nc := &karpv1.NodeClaim{Status: karpv1.NodeClaimStatus{ProviderID: ""}}

	err := (&CloudProvider{}).Delete(context.Background(), nc)

	require.True(t, karpcloudprovider.IsNodeClaimNotFoundError(err),
		"empty providerID must signal NotFound so karpenter clears the finalizer; got %v", err)
}

func variantInstanceType(name string, count string) *karpcloudprovider.InstanceType {
	return &karpcloudprovider.InstanceType{
		Name: name,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, count),
		),
	}
}

func instanceWithSSDLabel(instanceType, count string) *instance.Instance {
	return &instance.Instance{
		Type: instanceType,
		Labels: map[string]string{
			utils.SanitizeGCELabelValue(v1alpha1.LabelInstanceLocalSsdCount): count,
		},
	}
}

func TestInstanceTypesForScheduling(t *testing.T) {
	t.Parallel()

	catalog := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
		variantInstanceType("n2d-standard-8", "2"),
		variantInstanceType("n2d-standard-8", "4"),
		variantInstanceType("c4-standard-4-lssd", "1"),
		variantInstanceType("e2-standard-4", "0"),
	}
	counts := func(instanceTypes []*karpcloudprovider.InstanceType) []string {
		result := make([]string, 0, len(instanceTypes))
		for _, instanceType := range instanceTypes {
			result = append(result, instanceType.Name+":"+instanceType.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount).Any())
		}
		return result
	}

	t.Run("key absent exposes configurable count zero and keeps fixed shapes", func(t *testing.T) {
		t.Parallel()
		got := instanceTypesForScheduling(&karpv1.NodePool{}, catalog)
		require.Equal(t, []string{"n2d-standard-8:0", "c4-standard-4-lssd:1", "e2-standard-4:0"}, counts(got))
		require.Len(t, catalog, 5, "the cached full catalog must not be mutated")
	})

	t.Run("spec requirement declares configurable SSD opt in", func(t *testing.T) {
		t.Parallel()
		pool := &karpv1.NodePool{Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{Requirements: []karpv1.NodeSelectorRequirementWithMinValues{{
			Key: v1alpha1.LabelInstanceLocalSsdCount, Operator: corev1.NodeSelectorOpGt, Values: []string{"0"},
		}}}}}}
		got := instanceTypesForScheduling(pool, catalog)
		require.Equal(t, counts(catalog), counts(got))
	})

	t.Run("template label declares configurable SSD opt in", func(t *testing.T) {
		t.Parallel()
		pool := &karpv1.NodePool{Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{
			ObjectMeta: karpv1.ObjectMeta{Labels: map[string]string{v1alpha1.LabelInstanceLocalSsdCount: "4"}},
		}}}
		got := instanceTypesForScheduling(pool, catalog)
		require.Equal(t, counts(catalog), counts(got))
	})
}

func TestMatchVariantForInstance_ByGCELabel(t *testing.T) {
	t.Parallel()
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
		variantInstanceType("n2d-standard-8", "1"),
		variantInstanceType("n2d-standard-8", "2"),
		variantInstanceType("n2d-standard-8", "4"),
	}
	got, ok := matchVariantForInstance(its, instanceWithSSDLabel("n2d-standard-8", "2"))
	require.True(t, ok)
	req := got.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount)
	require.Equal(t, "2", req.Any(),
		"must pick the variant whose ssd-count requirement matches the instance's GCE label")
}

func TestMatchVariantForInstance_AbsentLabelFallsBackToFirstNameMatch(t *testing.T) {
	t.Parallel()
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
		variantInstanceType("n2d-standard-8", "2"),
	}
	inst := &instance.Instance{Type: "n2d-standard-8"}
	got, ok := matchVariantForInstance(its, inst)
	require.True(t, ok)
	req := got.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount)
	require.Equal(t, "0", req.Any(),
		"missing SSD-count label must fall back to the first Name-match variant")
}

func TestMatchVariantForInstance_UnknownLabelValueFallsBack(t *testing.T) {
	t.Parallel()
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
		variantInstanceType("n2d-standard-8", "2"),
	}
	got, ok := matchVariantForInstance(its, instanceWithSSDLabel("n2d-standard-8", "99"))
	require.True(t, ok)
	req := got.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount)
	require.Equal(t, "0", req.Any())
}

func TestMatchVariantForInstance_BundledSKUSingleVariant(t *testing.T) {
	t.Parallel()
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("c4d-standard-8-lssd", "1"),
	}
	got, ok := matchVariantForInstance(its, instanceWithSSDLabel("c4d-standard-8-lssd", "1"))
	require.True(t, ok)
	require.Equal(t, "c4d-standard-8-lssd", got.Name)
}

func TestMatchVariantForInstance_NoNameMatch(t *testing.T) {
	t.Parallel()
	its := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-8", "0"),
	}
	got, ok := matchVariantForInstance(its, instanceWithSSDLabel("x9-standard-2", "0"))
	require.False(t, ok)
	require.Nil(t, got)
}

func TestRepairPolicies_NPDConditionsPolarity(t *testing.T) {
	t.Parallel()
	// GKE Node Problem Detector conditions use True=problem polarity (opposite of NodeReady).
	// NPD sets a condition to True when a problem is detected and omits it otherwise.
	// ConditionFalse would never match and ConditionTrue must be used to trigger repair.
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
