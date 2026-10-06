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

package gke

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	containerv1 "google.golang.org/api/container/v1"
)

func TestClusterPodRangeNames(t *testing.T) {
	t.Parallel()

	cluster := &containerv1.Cluster{
		IpAllocationPolicy: &containerv1.IPAllocationPolicy{
			ClusterSecondaryRangeName: "cluster-pods",
		},
	}

	require.Equal(t, []string{"cluster-pods"}, ClusterPodRangeNames(cluster))
	require.Empty(t, ClusterPodRangeNames(&containerv1.Cluster{}))
	require.Empty(t, ClusterPodRangeNames(nil))
}

func TestClusterPodRangeNamesAdditionalRanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy *containerv1.IPAllocationPolicy
		want   []string
	}{
		{
			name: "configured names without utilization",
			policy: &containerv1.IPAllocationPolicy{
				ClusterSecondaryRangeName: "primary",
				AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{PodRangeNames: []string{"a", "b"}},
			},
			want: []string{"primary", "a", "b"},
		},
		{
			name: "utilization metadata only",
			policy: &containerv1.IPAllocationPolicy{
				ClusterSecondaryRangeName: "primary",
				AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{PodRangeInfo: []*containerv1.RangeInfo{{RangeName: "a"}}},
			},
			want: []string{"primary", "a"},
		},
		{
			name: "stable union of disagreeing fields",
			policy: &containerv1.IPAllocationPolicy{
				ClusterSecondaryRangeName: "primary",
				AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{
					PodRangeNames: []string{"b", "primary", "", "a", "b"},
					PodRangeInfo:  []*containerv1.RangeInfo{nil, {RangeName: ""}, {RangeName: "a"}, {RangeName: "c"}, {RangeName: "primary"}},
				},
			},
			want: []string{"primary", "b", "a", "c"},
		},
		{
			name: "additional ranges without primary name",
			policy: &containerv1.IPAllocationPolicy{
				AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{PodRangeNames: []string{"a"}},
			},
			want: []string{"a"},
		},
		{
			name: "separate subnetworks excluded",
			policy: &containerv1.IPAllocationPolicy{
				ClusterSecondaryRangeName: "primary",
				AdditionalIpRangesConfigs: []*containerv1.AdditionalIPRangesConfig{{
					Subnetwork: "other-subnet", PodIpv4RangeNames: []string{"other-pods"},
				}},
			},
			want: []string{"primary"},
		},
		{
			name: "no named ranges",
			policy: &containerv1.IPAllocationPolicy{
				AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{PodRangeNames: []string{""}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cluster := &containerv1.Cluster{IpAllocationPolicy: tt.policy}
			before, err := json.Marshal(cluster)
			require.NoError(t, err)

			require.Equal(t, tt.want, ClusterPodRangeNames(cluster))
			after, err := json.Marshal(cluster)
			require.NoError(t, err)
			require.Equal(t, before, after, "discovery must not mutate the cluster")
		})
	}
}
