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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestParseSourceKubeletArgsDropsTargetOwnedFields(t *testing.T) {
	args := ParseSourceKubeletArgs("--v=2 --max-pods=110 --node-labels=b=2,a=old --register-with-taints=source=true:NoSchedule --experimental-flag=value")

	require.Equal(t, []string{"--v=2", "--experimental-flag=value"}, args.other)
	require.Nil(t, args.maxPods)
	require.Empty(t, args.nodeLabels)
	require.Empty(t, args.nodeTaints)
	require.NotContains(t, args.String(), "--node-labels=")
	require.NotContains(t, args.String(), "--register-with-taints=")
}

func TestKubeletArgsStringRendersBootstrapFlags(t *testing.T) {
	maxPods := int32(32)
	args := KubeletArgs{
		other:      []string{"--v=2"},
		maxPods:    &maxPods,
		nodeLabels: Labels{"b": "2", "a": "1"},
		nodeTaints: Taints{
			{Key: "a", Value: "1", Effect: corev1.TaintEffectNoSchedule},
			{Key: "z", Effect: corev1.TaintEffectNoExecute},
		},
	}

	require.Equal(t, "--v=2 --max-pods=32 --node-labels=a=1,b=2 --register-with-taints=a=1:NoSchedule,z:NoExecute", args.String())
}

func TestKubeletArgsEmptyKnownFlagsAreOmitted(t *testing.T) {
	args := KubeletArgs{}

	require.Empty(t, args.String())
}

func TestKubeletArgsNodeTaintHelpers(t *testing.T) {
	args := KubeletArgs{}

	args.SetNodeTaints(Taints{{Key: "a", Value: "true", Effect: corev1.TaintEffectNoSchedule}})
	args.AppendNodeTaint(corev1.Taint{Key: "b", Effect: corev1.TaintEffectNoExecute})
	args.AppendNodeTaint(corev1.Taint{Key: "a", Value: "ignored", Effect: corev1.TaintEffectNoSchedule})

	require.Equal(t, "--register-with-taints=a=true:NoSchedule,b:NoExecute,a=ignored:NoSchedule", args.String())

}

func TestInstanceMetadataNodeTaintHelpersHideKubeletArgsStorage(t *testing.T) {
	metadata := InstanceMetadata{}

	metadata.SetNodeTaints(Taints{{Key: "a", Value: "1", Effect: corev1.TaintEffectNoSchedule}})

	require.Equal(t, Taints{{Key: "a", Value: "1", Effect: corev1.TaintEffectNoSchedule}}, metadata.kubeEnv.kubeletArgs.nodeTaints)
}
