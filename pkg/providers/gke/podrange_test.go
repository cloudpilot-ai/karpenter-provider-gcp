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
	"testing"

	"github.com/stretchr/testify/require"
	containerv1 "google.golang.org/api/container/v1"
)

func TestDefaultPodRangeName(t *testing.T) {
	t.Parallel()

	cluster := &containerv1.Cluster{
		IpAllocationPolicy: &containerv1.IPAllocationPolicy{
			ClusterSecondaryRangeName: "cluster-pods",
		},
	}

	require.Equal(t, "cluster-pods", DefaultPodRangeName(cluster))
	require.Equal(t, "", DefaultPodRangeName(&containerv1.Cluster{}))
	require.Equal(t, "", DefaultPodRangeName(nil))
}

func TestPodRangeUtilization(t *testing.T) {
	t.Parallel()

	cluster := &containerv1.Cluster{
		IpAllocationPolicy: &containerv1.IPAllocationPolicy{
			ClusterSecondaryRangeName:      "default-pods",
			DefaultPodIpv4RangeUtilization: 0.1,
			AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{
				PodRangeInfo: []*containerv1.RangeInfo{
					{RangeName: "extra", Utilization: 0.9},
				},
			},
		},
	}

	util, ok := PodRangeUtilization(cluster, "default-pods")
	require.True(t, ok)
	require.Equal(t, 0.1, util)

	util, ok = PodRangeUtilization(cluster, "extra")
	require.True(t, ok)
	require.Equal(t, 0.9, util)

	_, ok = PodRangeUtilization(cluster, "missing")
	require.False(t, ok)
}
