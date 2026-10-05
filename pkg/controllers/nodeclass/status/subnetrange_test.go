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

package status

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	containerv1 "google.golang.org/api/container/v1"
	"k8s.io/utils/ptr"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

type stubGKEProvider struct {
	cluster *containerv1.Cluster
}

func (s *stubGKEProvider) ResolveClusterZones(context.Context) ([]string, error) {
	return nil, nil
}

func (s *stubGKEProvider) GetClusterConfig(context.Context) (*containerv1.Cluster, error) {
	return s.cluster, nil
}

func (s *stubGKEProvider) GetServerConfig(context.Context) (*containerv1.ServerConfig, error) {
	return &containerv1.ServerConfig{}, nil
}

func TestSubnetRangeStatus(t *testing.T) {
	t.Parallel()

	cluster := &containerv1.Cluster{
		IpAllocationPolicy: &containerv1.IPAllocationPolicy{
			ClusterSecondaryRangeName:      "default-pods",
			DefaultPodIpv4RangeUtilization: 0.4,
			AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{
				PodRangeInfo: []*containerv1.RangeInfo{
					{RangeName: "extra-pods", Utilization: 0.1},
				},
			},
		},
	}
	r := &SubnetRange{gkeProvider: &stubGKEProvider{cluster: cluster}}
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		SubnetRangeNames: []string{"default-pods", "extra-pods"},
	}}

	_, err := r.Reconcile(context.Background(), nc)
	require.NoError(t, err)
	require.Equal(t, []v1alpha1.SubnetRangeStatus{
		{Name: "default-pods", Utilization: ptr.To("0.4")},
		{Name: "extra-pods", Utilization: ptr.To("0.1")},
	}, nc.Status.SubnetRanges)
}

func TestSubnetRangeStatusDiscoveryAndOverrides(t *testing.T) {
	t.Parallel()
	cluster := &containerv1.Cluster{
		IpAllocationPolicy: &containerv1.IPAllocationPolicy{
			ClusterSecondaryRangeName:      "default-pods",
			DefaultPodIpv4RangeUtilization: 0.4,
			AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{
				PodRangeNames: []string{"unreported-pods"},
				PodRangeInfo: []*containerv1.RangeInfo{
					{RangeName: "extra-pods", Utilization: 0.1},
				},
			},
		},
	}
	tests := []struct {
		name string
		spec v1alpha1.GCENodeClassSpec
		want []v1alpha1.SubnetRangeStatus
	}{
		{
			name: "omitted fields discover all ranges",
			want: []v1alpha1.SubnetRangeStatus{
				{Name: "default-pods", Utilization: ptr.To("0.4")},
				{Name: "unreported-pods"},
				{Name: "extra-pods", Utilization: ptr.To("0.1")},
			},
		},
		{
			name: "list replaces discovery",
			spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"extra-pods"}},
			want: []v1alpha1.SubnetRangeStatus{{Name: "extra-pods", Utilization: ptr.To("0.1")}},
		},
		{
			name: "deprecated scalar replaces discovery",
			spec: v1alpha1.GCENodeClassSpec{
				SubnetRangeName: ptr.To("extra-pods"), //nolint:staticcheck // Verify the deprecated override remains supported.
			},
			want: []v1alpha1.SubnetRangeStatus{{Name: "extra-pods", Utilization: ptr.To("0.1")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &SubnetRange{gkeProvider: &stubGKEProvider{cluster: cluster}}
			nc := &v1alpha1.GCENodeClass{Spec: tt.spec}

			_, err := r.Reconcile(context.Background(), nc)

			require.NoError(t, err)
			require.Equal(t, tt.want, nc.Status.SubnetRanges)
		})
	}
}
