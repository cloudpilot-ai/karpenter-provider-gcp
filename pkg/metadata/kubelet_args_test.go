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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestParseKubeletArgsPreservesSourceTemplateBoundaryFields(t *testing.T) {
	args := ParseKubeletArgs("--v=2 --max-pods=110 --node-labels=b=2,a=old --register-with-taints=source=true:NoSchedule --node-labels=a=new --experimental-flag=value")

	require.Equal(t, []string{"--v=2", "--experimental-flag=value"}, args.other)
	require.NotNil(t, args.maxPods)
	require.Equal(t, int32(110), *args.maxPods)
	require.Equal(t, Labels{"a": "new", "b": "2"}, args.nodeLabels)
	require.Empty(t, args.nodeTaints)
	require.NotContains(t, args.String(), "source=true:NoSchedule")
}

func TestParseKubeletArgsInvalidMaxPodsPreservedInOther(t *testing.T) {
	args := ParseKubeletArgs("before --max-pods=abc after")

	require.Nil(t, args.maxPods)
	require.Equal(t, []string{"before", "--max-pods=abc", "after"}, args.other)
	require.Equal(t, "before --max-pods=abc after", args.String())
}

func TestKubeletArgsStringRendersDeterministically(t *testing.T) {
	maxPods := int32(32)
	args := KubeletArgs{
		other:      []string{"--v=2"},
		maxPods:    &maxPods,
		nodeLabels: Labels{"b": "2", "a": "1"},
		nodeTaints: Taints{
			{Key: "z", Effect: corev1.TaintEffectNoExecute},
			{Key: "a", Value: "1", Effect: corev1.TaintEffectNoSchedule},
		},
	}

	require.Equal(t, "--v=2 --max-pods=32 --node-labels=a=1,b=2 --register-with-taints=a=1:NoSchedule,z:NoExecute", args.String())
}

func TestKubeletArgsEmptyKnownFlagsAreOmitted(t *testing.T) {
	args := KubeletArgs{}

	require.Empty(t, args.String())
}

func TestKubeletArgsNodeTaintHelpers(t *testing.T) {
	args := KubeletArgs{nodeTaints: Taints{{Key: "a", Value: "true", Effect: corev1.TaintEffectNoSchedule}}}

	args.AddNodeTaints(
		corev1.Taint{Key: "b", Effect: corev1.TaintEffectNoExecute},
		corev1.Taint{Key: "a", Value: "ignored", Effect: corev1.TaintEffectNoSchedule},
	)

	require.Equal(t, "--register-with-taints=a=true:NoSchedule,b:NoExecute", args.String())

	args.SetNodeTaints(corev1.Taint{Key: "c", Value: "1", Effect: corev1.TaintEffectNoSchedule})
	require.Equal(t, "--register-with-taints=c=1:NoSchedule", args.String())
}

func TestTaintsStringRendersDeterministically(t *testing.T) {
	taints := Taints{
		{Key: "z", Effect: corev1.TaintEffectNoExecute},
		{Key: "a", Value: "1", Effect: corev1.TaintEffectNoSchedule},
		{Key: "a", Value: "ignored", Effect: corev1.TaintEffectNoSchedule},
		{Key: "", Effect: corev1.TaintEffectNoSchedule},
	}

	require.Equal(t, "a=1:NoSchedule,z:NoExecute", taints.String())
}

func TestInstanceMetadataNodeTaintHelpersHideKubeletArgsStorage(t *testing.T) {
	metadata := InstanceMetadata{}

	metadata.SetNodeTaints(corev1.Taint{Key: "a", Value: "1", Effect: corev1.TaintEffectNoSchedule})
	metadata.AddNodeTaints(
		corev1.Taint{Key: "b", Effect: corev1.TaintEffectNoExecute},
		corev1.Taint{Key: "a", Value: "ignored", Effect: corev1.TaintEffectNoSchedule},
	)

	require.Equal(t, Taints{
		{Key: "a", Value: "1", Effect: corev1.TaintEffectNoSchedule},
		{Key: "b", Effect: corev1.TaintEffectNoExecute},
	}, metadata.kubeEnv.kubeletArgs.nodeTaints)
	require.Equal(t, 1, strings.Count(metadata.kubeEnv.kubeletArgs.String(), "--register-with-taints="))
}
