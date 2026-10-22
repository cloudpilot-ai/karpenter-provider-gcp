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
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestAdmittedPodsUseUpstreamRuntimeRequirementsAndPermanentTaints(t *testing.T) {
	runtimeSelector := map[string]string{v1alpha1.LabelSandboxRuntime: "gvisor"}
	toleration := corev1.Toleration{Key: v1alpha1.LabelSandboxRuntime, Value: "gvisor", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule}
	for _, tc := range []struct {
		name              string
		selector          map[string]string
		tolerations       []corev1.Toleration
		customClass       *string
		ordinary, sandbox bool
	}{
		{name: "ordinary Pod", ordinary: true},
		{name: "admitted custom RuntimeClass", selector: runtimeSelector, tolerations: []corev1.Toleration{toleration}, customClass: lo.ToPtr("custom-handler-gvisor"), sandbox: true},
		{name: "runtime selector without toleration", selector: runtimeSelector},
		{name: "regular Pod explicitly tolerates sandbox", tolerations: []corev1.Toleration{toleration}, ordinary: true, sandbox: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{NodeSelector: tc.selector, Tolerations: tc.tolerations, RuntimeClassName: tc.customClass}}
			for _, enabled := range []bool{false, true} {
				requirements := scheduling.NewRequirements(RuntimeRequirement(enabled))
				var taints scheduling.Taints
				if enabled {
					taints = scheduling.Taints{RuntimeTaint()}
				}
				compatible := requirements.Compatible(scheduling.NewPodRequirements(pod)) == nil && taints.ToleratesPod(pod) == nil
				expected := tc.ordinary
				if enabled {
					expected = tc.sandbox
				}
				require.Equal(t, expected, compatible)
			}
		})
	}
}

func TestRuntimeContractRejectsContradictoryRequirementsAndStartupIsolation(t *testing.T) {
	pool := &karpv1.NodePool{}
	pool.Spec.Template.Labels = map[string]string{v1alpha1.LabelSandboxRuntime: "gvisor"}
	pool.Spec.Template.Spec.Taints = []corev1.Taint{RuntimeTaint()}
	pool.Spec.Template.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{{Key: v1alpha1.LabelSandboxRuntime, Operator: corev1.NodeSelectorOpDoesNotExist}}
	require.Error(t, ValidatePool(pool, true))
	pool.Spec.Template.Spec.Requirements = nil
	pool.Spec.Template.Spec.StartupTaints = pool.Spec.Template.Spec.Taints
	pool.Spec.Template.Spec.Taints = nil
	require.Error(t, ValidatePool(pool, true))
	require.Error(t, ValidatePool(pool, false))
}
