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

package subnet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
	"k8s.io/utils/clock"
)

func testClient(t *testing.T, handler http.Handler) *compute.SubnetworksClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := compute.NewSubnetworksRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client
}

func TestGetFreeIPCounts(t *testing.T) {
	t.Parallel()
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/compute/v1/projects/host/regions/us-central1/subnetworks/pods", r.URL.Path)
		require.Equal(t, "WITH_UTILIZATION", r.URL.Query().Get("views"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"utilizationDetails":{"ipv4Utilizations":[
			{"rangeName":"","totalFreeIp":"100000"},
			{"rangeName":"available","totalFreeIp":"1024"},
			{"rangeName":"exhausted","totalFreeIp":"0"},
			{"rangeName":"unreported"}
		]}}`)
	}))
	p := NewProvider(client, "us-central1", clock.RealClock{})

	counts, err := p.GetFreeIPCounts(context.Background(), "", "projects/host/regions/us-central1/subnetworks/pods")

	require.NoError(t, err)
	require.Equal(t, map[string]int64{"available": 1024, "exhausted": 0}, counts)
}

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time                  { return c.now }
func (c *testClock) Since(t time.Time) time.Duration { return c.now.Sub(t) }

func TestFreeIPCacheExpiresAndDoesNotServeStaleCounts(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := requests.Add(1)
		if n == 2 {
			http.Error(w, `{"error":{"code":403,"message":"denied"}}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"utilizationDetails":{"ipv4Utilizations":[{"rangeName":"pods","totalFreeIp":"%d"}]}}`, n*10)
	}))
	fakeClock := &testClock{now: time.Now()}
	p := NewProvider(client, "us-central1", fakeClock)
	counts, err := p.GetFreeIPCounts(context.Background(), "", "projects/host/regions/us-central1/subnetworks/pods")
	require.NoError(t, err)
	counts["pods"] = 999

	cached, err := p.GetFreeIPCounts(context.Background(), "projects/host/global/networks/vpc", "pods")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"pods": 10}, cached, "caller mutations must not alter the cached snapshot")
	require.Equal(t, int32(1), requests.Load(), "equivalent targets must share the cache")

	fakeClock.now = fakeClock.now.Add(time.Minute)
	counts, err = p.GetFreeIPCounts(context.Background(), "", "projects/host/regions/us-central1/subnetworks/pods")
	require.Error(t, err)
	require.Nil(t, counts, "expired counts must not survive a failed refresh")

	counts, err = p.GetFreeIPCounts(context.Background(), "", "projects/host/regions/us-central1/subnetworks/pods")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"pods": 30}, counts, "failed lookups must not be cached")
}

func TestFreeIPLookupResolvesSubnetworkOwnership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, network, subnetwork, wantPath string
	}{
		{name: "Shared VPC self link", subnetwork: "https://www.googleapis.com/compute/v1/projects/host/regions/us-east1/subnetworks/pods", wantPath: "/compute/v1/projects/host/regions/us-east1/subnetworks/pods"},
		{name: "Shared VPC bare name", network: "https://www.googleapis.com/compute/v1/projects/host/global/networks/vpc", subnetwork: "pods", wantPath: "/compute/v1/projects/host/regions/us-central1/subnetworks/pods"},
		{name: "relative region", network: "projects/host/global/networks/vpc", subnetwork: "regions/us-east1/subnetworks/pods", wantPath: "/compute/v1/projects/host/regions/us-east1/subnetworks/pods"},
		{name: "ambiguous project", network: "vpc", subnetwork: "pods"},
		{name: "invalid reference", subnetwork: "projects/host/zones/us-central1-a/subnetworks/pods"},
		{name: "empty project", subnetwork: "projects//regions/us-central1/subnetworks/pods"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				require.Equal(t, tt.wantPath, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{}`)
			}))
			p := NewProvider(client, "us-central1", clock.RealClock{})

			counts, err := p.GetFreeIPCounts(context.Background(), tt.network, tt.subnetwork)

			if tt.wantPath == "" {
				require.Error(t, err)
				require.Nil(t, counts)
				require.Zero(t, requests.Load(), "ambiguous or invalid targets must not be queried")
			} else {
				require.NoError(t, err)
				require.Empty(t, counts, "missing utilization details are unknown")
			}
		})
	}
}

func TestFreeIPCacheSeparatesSubnetworks(t *testing.T) {
	t.Parallel()
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := 10
		if r.URL.Path == "/compute/v1/projects/host/regions/us-central1/subnetworks/second" {
			count = 20
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"utilizationDetails":{"ipv4Utilizations":[{"rangeName":"pods","totalFreeIp":"%d"}]}}`, count)
	}))
	p := NewProvider(client, "us-central1", clock.RealClock{})
	for name, want := range map[string]int64{"first": 10, "second": 20} {
		counts, err := p.GetFreeIPCounts(context.Background(), "projects/host/global/networks/vpc", name)
		require.NoError(t, err)
		require.Equal(t, want, counts["pods"], "the same range name belongs to distinct subnet snapshots")
	}
}

func TestFreeIPCacheAllowsConcurrentCallerMutation(t *testing.T) {
	t.Parallel()
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"utilizationDetails":{"ipv4Utilizations":[{"rangeName":"pods","totalFreeIp":"400"}]}}`)
	}))
	p := NewProvider(client, "us-central1", clock.RealClock{})
	_, err := p.GetFreeIPCounts(context.Background(), "projects/host/global/networks/vpc", "pods")
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			counts, err := p.GetFreeIPCounts(context.Background(), "projects/host/global/networks/vpc", "pods")
			require.NoError(t, err)
			require.Equal(t, int64(400), counts["pods"])
			counts["pods"] = 0
		})
	}
	wg.Wait()
}

func TestFreeIPLookupHonorsCallerCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := testClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	p := NewProvider(client, "us-central1", clock.RealClock{})

	counts, err := p.GetFreeIPCounts(ctx, "projects/host/global/networks/vpc", "pods")

	require.Nil(t, counts)
	require.True(t, errors.Is(err, context.Canceled), "got %v", err)
}

func TestFreeIPLookupIsBounded(t *testing.T) {
	t.Parallel()
	client := testClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	p := NewProvider(client, "us-central1", clock.RealClock{})

	counts, err := p.GetFreeIPCounts(context.Background(), "projects/host/global/networks/vpc", "pods")

	require.Nil(t, counts)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
}
