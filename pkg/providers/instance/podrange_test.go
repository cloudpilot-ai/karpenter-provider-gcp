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

package instance

import (
	"testing"

	"github.com/stretchr/testify/require"
	containerv1 "google.golang.org/api/container/v1"
	"k8s.io/utils/ptr"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestResolvedPodRangeNames(t *testing.T) {
	t.Parallel()

	cluster := &containerv1.Cluster{
		IpAllocationPolicy: &containerv1.IPAllocationPolicy{
			ClusterSecondaryRangeName: "cluster-pods",
		},
	}

	t.Run("list field", func(t *testing.T) {
		t.Parallel()
		nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			SubnetRangeNames: []string{"a", "b"},
		}}
		require.Equal(t, []string{"a", "b"}, resolvedPodRangeNames(nc, cluster))
	})

	t.Run("scalar field", func(t *testing.T) {
		t.Parallel()
		nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			SubnetRangeName: ptr.To("custom"), //nolint:staticcheck // Verify backward compatibility with the deprecated field.
		}}
		require.Equal(t, []string{"custom"}, resolvedPodRangeNames(nc, cluster))
	})

	t.Run("cluster default", func(t *testing.T) {
		t.Parallel()
		nc := &v1alpha1.GCENodeClass{}
		require.Equal(t, []string{"cluster-pods"}, resolvedPodRangeNames(nc, cluster))
	})

	t.Run("empty when cluster has no default", func(t *testing.T) {
		t.Parallel()
		nc := &v1alpha1.GCENodeClass{}
		require.Equal(t, []string{""}, resolvedPodRangeNames(nc, &containerv1.Cluster{}))
	})
}

func TestRankPodRangeNames(t *testing.T) {
	t.Parallel()

	counts := map[string]int64{"default-pods": 0, "extra-a": 8000, "extra-b": 1000}
	names := []string{"default-pods", "extra-b", "extra-a", "unknown"}
	ranked := rankPodRangeNames(names, counts)
	require.Equal(t, []string{"default-pods", "extra-b", "extra-a", "unknown"}, names, "ranking must not mutate candidates")
	require.Equal(t, []string{"extra-a", "extra-b", "default-pods", "unknown"}, ranked)
}

func TestRankPodRangeNamesSpecOrderTieBreak(t *testing.T) {
	t.Parallel()

	counts := map[string]int64{"a": 1000, "b": 1000}
	require.Equal(t, []string{"a", "b"}, rankPodRangeNames([]string{"a", "b"}, counts))
	require.Equal(t, []string{"b", "a"}, rankPodRangeNames([]string{"b", "a"}, counts))
	require.Equal(t, []string{"b", "a"}, rankPodRangeNames([]string{"b", "a"}, nil))
}
