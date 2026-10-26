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

package sandbox

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func RuntimeRequirement(enabled bool) *scheduling.Requirement {
	if enabled {
		return scheduling.NewRequirement(v1alpha1.LabelSandboxRuntime, corev1.NodeSelectorOpIn, v1alpha1.SandboxRuntimeGVISOR)
	}
	return scheduling.NewRequirement(v1alpha1.LabelSandboxRuntime, corev1.NodeSelectorOpDoesNotExist)
}

func RuntimeTaint() corev1.Taint {
	return corev1.Taint{Key: v1alpha1.LabelSandboxRuntime, Value: v1alpha1.SandboxRuntimeGVISOR, Effect: corev1.TaintEffectNoSchedule}
}

// ValidatePool checks persisted scheduling fields, not an in-memory repair.
func ValidatePool(pool *karpv1.NodePool, enabled bool) error {
	return validateFields(pool.Spec.Template.Labels, pool.Spec.Template.Spec.Taints, pool.Spec.Template.Spec.StartupTaints, pool.Spec.Template.Spec.Requirements, enabled)
}

func ValidateClaim(claim *karpv1.NodeClaim, enabled bool) error {
	if err := validateFields(claim.Labels, claim.Spec.Taints, claim.Spec.StartupTaints, claim.Spec.Requirements, enabled); err != nil {
		return err
	}
	if enabled {
		req := scheduling.NewNodeSelectorRequirementsWithMinValues(claim.Spec.Requirements...).Get(v1alpha1.LabelSandboxRuntime)
		if req.Operator() != corev1.NodeSelectorOpIn || req.Len() != 1 || req.Any() != v1alpha1.SandboxRuntimeGVISOR {
			return fmt.Errorf("sandbox NodeClaim requires an immutable In gvisor requirement")
		}
	}
	return nil
}

func validateFields(labels map[string]string, taints, startup []corev1.Taint, requirements []karpv1.NodeSelectorRequirementWithMinValues, enabled bool) error {
	value, present := labels[v1alpha1.LabelSandboxRuntime]
	if present != enabled || (enabled && value != v1alpha1.SandboxRuntimeGVISOR) {
		return fmt.Errorf("runtime label disagrees with GCENodeClass")
	}
	if err := validateTaints(taints, startup, enabled); err != nil {
		return err
	}
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(requirements...)
	if reqs.Has(v1alpha1.LabelSandboxRuntime) && scheduling.NewRequirements(RuntimeRequirement(enabled)).Compatible(reqs, scheduling.AllowUndefinedWellKnownLabels) != nil {
		return fmt.Errorf("runtime requirements contradict GCENodeClass")
	}
	return nil
}

func validateTaints(taints, startup []corev1.Taint, enabled bool) error {
	count := 0
	for _, taint := range taints {
		if taint.Key != v1alpha1.LabelSandboxRuntime {
			continue
		}
		count++
		if !enabled || taint.Value != v1alpha1.SandboxRuntimeGVISOR || taint.Effect != corev1.TaintEffectNoSchedule {
			return fmt.Errorf("runtime taint disagrees with GCENodeClass")
		}
	}
	if enabled && count != 1 {
		return fmt.Errorf("sandbox requires exactly one permanent runtime NoSchedule taint")
	}
	for _, taint := range startup {
		if taint.Key == v1alpha1.LabelSandboxRuntime {
			return fmt.Errorf("runtime isolation cannot be a startup taint")
		}
	}

	return nil
}
