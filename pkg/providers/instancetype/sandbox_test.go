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

package instancetype

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
)

func TestListSeparatesRuntimeCachesWithoutChangingCapacityOrOverhead(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.07})
	provider := newTestProvider()
	ordinary := &v1alpha1.GCENodeClass{}
	sandbox := ordinary.DeepCopy()
	sandbox.Spec.SandboxConfig = &v1alpha1.SandboxConfig{Type: "GVISOR"}
	first, err := provider.List(ctx, ordinary)
	require.NoError(t, err)
	enabled, err := provider.List(ctx, sandbox)
	require.NoError(t, err)
	again, err := provider.List(ctx, ordinary)
	require.NoError(t, err)
	require.NotEmpty(t, enabled)
	for i, it := range enabled {
		require.Equal(t, corev1.NodeSelectorOpIn, it.Requirements.Get(v1alpha1.LabelSandboxRuntime).Operator())
		require.Equal(t, "gvisor", it.Requirements.Get(v1alpha1.LabelSandboxRuntime).Any())
		require.Equal(t, corev1.NodeSelectorOpDoesNotExist, first[i].Requirements.Get(v1alpha1.LabelSandboxRuntime).Operator())
		require.Equal(t, corev1.NodeSelectorOpDoesNotExist, again[i].Requirements.Get(v1alpha1.LabelSandboxRuntime).Operator())
		require.Equal(t, first[i].Capacity, it.Capacity, "SMT remains enabled and runtime overhead belongs to admission/core")
		require.Equal(t, first[i].Overhead, it.Overhead)
	}
}
