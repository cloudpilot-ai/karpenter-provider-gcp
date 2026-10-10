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

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func TestInstanceToNodeClaimPropagatesBareMetalLabel(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	for _, tt := range []struct {
		name      string
		bareMetal string
	}{
		{name: "c4a-highmem-96-metal", bareMetal: "true"},
		{name: "c4a-highmem-96", bareMetal: "false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mt := &computepb.MachineType{
				Name:      lo.ToPtr(tt.name),
				GuestCpus: lo.ToPtr[int32](96),
				MemoryMb:  lo.ToPtr[int32](768 * 1024),
			}
			it := instancetype.NewInstanceType(ctx, mt, &v1alpha1.GCENodeClass{}, "us-central1", karpcloudprovider.Offerings{}, 0)
			require.NotNil(t, it)
			nc := (&CloudProvider{}).instanceToNodeClaim(&instance.Instance{Type: tt.name, Location: "us-central1-a"}, it)
			require.Equal(t, tt.bareMetal, nc.Labels[v1alpha1.LabelInstanceBareMetal],
				"the computed selector must reach the NodeClaim so core can copy it to the Node")
		})
	}
}

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

type existingInstanceProvider struct {
	instance.Provider
	inst *instance.Instance
}

func (p existingInstanceProvider) Create(context.Context, *v1alpha1.GCENodeClass, *karpv1.NodeClaim, []*karpcloudprovider.InstanceType) (*instance.Instance, error) {
	return p.inst, nil
}

func TestCreateMatchesAdoptedCountOutsideFilteredCandidates(t *testing.T) {
	t.Parallel()

	zero := variantInstanceType("n2d-standard-8", "0")
	two := variantInstanceType("n2d-standard-8", "2")
	for _, it := range []*karpcloudprovider.InstanceType{zero, two} {
		it.Overhead = &karpcloudprovider.InstanceTypeOverhead{}
		it.Offerings = karpcloudprovider.Offerings{&karpcloudprovider.Offering{Available: true, Requirements: scheduling.NewRequirements()}}
	}
	zero.Capacity = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("50Gi")}
	two.Capacity = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("700Gi")}

	nodeClass := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
	claim := &karpv1.NodeClaim{Spec: karpv1.NodeClaimSpec{
		NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
		Resources:    karpv1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("80Gi")}},
	}}

	for _, tc := range []struct {
		name, count string
		wantError   bool
	}{
		{name: "count filtered by current resources", count: "0"},
		{name: "count absent from full catalog", count: "99", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inst := instanceWithSSDLabel("n2d-standard-8", tc.count)
			inst.Name = "karpenter-claim"
			provider := New(reproClient{nodeClass: nodeClass}, reproEvents{}, reproTypes{variants: []*karpcloudprovider.InstanceType{zero, two}}, existingInstanceProvider{inst: inst}, nil)
			got, err := provider.Create(karpopts.ToContext(context.Background(), &karpopts.Options{}), claim)
			if tc.wantError {
				require.ErrorContains(t, err, "local SSD count")
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "0", got.Labels[v1alpha1.LabelInstanceLocalSsdCount], "adoption must use the VM's count even when the current NodeClass filters its variant")
		})
	}
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

func TestRebootNotImplemented(t *testing.T) {
	t.Parallel()
	err := (&CloudProvider{}).Reboot(context.Background(), &karpv1.NodeClaim{}, "repair-operation")
	require.True(t, karpcloudprovider.IsNodeRebootNotImplementedError(err))
}

func TestRepairPoliciesAcceptedByCore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		enabled bool
		legacy  bool
	}{
		{name: "modern", enabled: true},
		{name: "disabled"},
		{name: "legacy", enabled: true, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := karpopts.ToContext(context.Background(), &karpopts.Options{
				FeatureGates: karpopts.FeatureGates{NodeRepair: tc.enabled}, LegacyNodeRepair: tc.legacy,
			})
			matcher, err := health.NewRepairPolicyMatcher(ctx, &CloudProvider{})
			require.NoError(t, err)
			if tc.enabled && !tc.legacy {
				require.NotNil(t, matcher)
			} else {
				require.Nil(t, matcher)
			}
			cluster := state.NewCluster(clock.RealClock{}, nil, &CloudProvider{}, state.WithRepairPolicyMatcher(matcher))
			require.NotPanics(t, func() {
				disruption.NewMethods(ctx, clock.RealClock{}, cluster, nil, nil, &CloudProvider{}, reproEvents{}, nil)
			})
		})
	}
}

func TestRepairPoliciesReplaceUnhealthyNodes(t *testing.T) {
	t.Parallel()
	ctx := karpopts.ToContext(context.Background(), &karpopts.Options{FeatureGates: karpopts.FeatureGates{NodeRepair: true}})
	matcher, err := health.NewRepairPolicyMatcher(ctx, &CloudProvider{})
	require.NoError(t, err)
	transition := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		condition corev1.NodeConditionType
		status    corev1.ConditionStatus
		wait      time.Duration
	}{
		{corev1.NodeReady, corev1.ConditionFalse, 10 * time.Minute},
		{corev1.NodeReady, corev1.ConditionUnknown, 10 * time.Minute},
		{"KernelDeadlock", corev1.ConditionTrue, 5 * time.Minute},
		{"ReadonlyFilesystem", corev1.ConditionTrue, 5 * time.Minute},
		{"FrequentKubeletRestart", corev1.ConditionTrue, 30 * time.Minute},
		{"FrequentContainerdRestart", corev1.ConditionTrue, 30 * time.Minute},
	} {
		t.Run(string(tc.condition)+"/"+string(tc.status), func(t *testing.T) {
			t.Parallel()
			for _, reason := range []string{"", "UnrecognizedGKEReason"} {
				node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
					Type: tc.condition, Status: tc.status, Reason: reason, LastTransitionTime: metav1.NewTime(transition),
				}}}}
				matches := matcher.Match(node)
				require.Empty(t, health.Resolve(matches, transition.Add(tc.wait-time.Second), time.Time{}).Action)
				result := health.Resolve(matches, transition.Add(tc.wait), time.Time{})
				require.Equal(t, karpcloudprovider.ReplaceNode, result.Action)
				require.NotNil(t, result.TerminationGracePeriod)
				require.Zero(t, *result.TerminationGracePeriod)
			}
		})
	}
	for _, condition := range []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		{Type: "KernelDeadlock", Status: corev1.ConditionFalse},
		{Type: "ReadonlyFilesystem", Status: corev1.ConditionUnknown},
		{Type: "FrequentKubeletRestart", Status: corev1.ConditionFalse},
		{Type: "FrequentContainerdRestart", Status: corev1.ConditionFalse},
		{Type: "UnsupportedHealthCondition", Status: corev1.ConditionTrue},
	} {
		require.Empty(t, matcher.Match(&corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{condition}}}))
	}
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
