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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"github.com/stretchr/testify/require"
	containerv1 "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/subnet"
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

func statusSubnetProvider(t *testing.T, handler http.Handler) subnet.Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := computeapi.NewSubnetworksRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return subnet.NewProvider(client, "us-central1", clock.RealClock{})
}

func statusCluster() *containerv1.Cluster {
	return &containerv1.Cluster{
		NetworkConfig: &containerv1.NetworkConfig{Network: "projects/host/global/networks/vpc", Subnetwork: "pods"},
		IpAllocationPolicy: &containerv1.IPAllocationPolicy{
			ClusterSecondaryRangeName: "default-pods",
			AdditionalPodRangesConfig: &containerv1.AdditionalPodRangesConfig{
				PodRangeNames: []string{"unreported-pods"},
				PodRangeInfo:  []*containerv1.RangeInfo{{RangeName: "extra-pods"}},
			},
		},
	}
}

func TestSubnetRangeStatusReportsFreeIPs(t *testing.T) {
	t.Parallel()
	p := statusSubnetProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/compute/v1/projects/host/regions/us-central1/subnetworks/pods", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"utilizationDetails":{"ipv4Utilizations":[
			{"rangeName":"available","totalFreeIp":"1024"},
			{"rangeName":"exhausted","totalFreeIp":"0"},
			{"rangeName":"unknown"}
		]}}`)
	}))
	r := &SubnetRange{gkeProvider: &stubGKEProvider{cluster: statusCluster()}, subnetProvider: p}
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"available", "exhausted", "unknown"}}}

	_, err := r.Reconcile(context.Background(), nc)
	require.NoError(t, err)
	statusJSON, err := json.Marshal(nc.Status.SubnetRanges)
	require.NoError(t, err)
	require.JSONEq(t, `[{"name":"available","totalFreeIP":1024},{"name":"exhausted","totalFreeIP":0},{"name":"unknown"}]`, string(statusJSON))
}

func TestSubnetRangeStatusDiscoveryAndOverrides(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec v1alpha1.GCENodeClassSpec
		want []v1alpha1.SubnetRangeStatus
	}{
		{
			name: "omitted fields discover all ranges",
			want: []v1alpha1.SubnetRangeStatus{
				{Name: "default-pods", TotalFreeIP: ptr.To(int64(400))},
				{Name: "unreported-pods"},
				{Name: "extra-pods", TotalFreeIP: ptr.To(int64(100))},
			},
		},
		{
			name: "list replaces discovery",
			spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"extra-pods"}},
			want: []v1alpha1.SubnetRangeStatus{{Name: "extra-pods", TotalFreeIP: ptr.To(int64(100))}},
		},
		{
			name: "deprecated scalar replaces discovery",
			spec: v1alpha1.GCENodeClassSpec{
				SubnetRangeName: ptr.To("extra-pods"), //nolint:staticcheck // Verify the deprecated override remains supported.
			},
			want: []v1alpha1.SubnetRangeStatus{{Name: "extra-pods", TotalFreeIP: ptr.To(int64(100))}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := statusSubnetProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"utilizationDetails":{"ipv4Utilizations":[
					{"rangeName":"default-pods","totalFreeIp":"400"},
					{"rangeName":"extra-pods","totalFreeIp":"100"},
					{"rangeName":"ineligible","totalFreeIp":"9999"}
				]}}`)
			}))
			r := &SubnetRange{gkeProvider: &stubGKEProvider{cluster: statusCluster()}, subnetProvider: p}
			nc := &v1alpha1.GCENodeClass{Spec: tt.spec}

			_, err := r.Reconcile(context.Background(), nc)

			require.NoError(t, err)
			require.Equal(t, tt.want, nc.Status.SubnetRanges)
		})
	}
}

func TestSubnetRangeStatusUsesSubnetworkOverride(t *testing.T) {
	t.Parallel()
	p := statusSubnetProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/compute/v1/projects/other-host/regions/us-east1/subnetworks/custom", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"utilizationDetails":{"ipv4Utilizations":[{"rangeName":"custom-pods","totalFreeIp":"400"}]}}`)
	}))
	r := &SubnetRange{gkeProvider: &stubGKEProvider{cluster: statusCluster()}, subnetProvider: p}
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		SubnetRangeNames: []string{"custom-pods"},
		NetworkConfig:    &v1alpha1.NetworkConfig{Subnetwork: "projects/other-host/regions/us-east1/subnetworks/custom"},
	}}

	_, err := r.Reconcile(context.Background(), nc)

	require.NoError(t, err)
	require.Equal(t, []v1alpha1.SubnetRangeStatus{{Name: "custom-pods", TotalFreeIP: ptr.To(int64(400))}}, nc.Status.SubnetRanges)
}

func TestSubnetRangeStatusFailureClearsCountsWithoutError(t *testing.T) {
	t.Parallel()
	p := statusSubnetProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":403,"message":"denied"}}`, http.StatusForbidden)
	}))
	r := &SubnetRange{gkeProvider: &stubGKEProvider{cluster: statusCluster()}, subnetProvider: p}
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"extra-pods"}}}
	nc.Status.SubnetRanges = []v1alpha1.SubnetRangeStatus{{Name: "extra-pods", TotalFreeIP: ptr.To(int64(100))}}

	result, err := r.Reconcile(context.Background(), nc)

	require.NoError(t, err)
	require.Equal(t, subnetRangeStatusRequeue, result.RequeueAfter)
	require.Equal(t, []v1alpha1.SubnetRangeStatus{{Name: "extra-pods"}}, nc.Status.SubnetRanges)
}
