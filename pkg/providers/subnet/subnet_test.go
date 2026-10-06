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

package subnet_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/subnet"
)

const testNetwork = "projects/test-project/global/networks/default"

type fakeSubnetworksClient struct {
	calls atomic.Int64
	get   func(context.Context, *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error)
}

func (c *fakeSubnetworksClient) Get(ctx context.Context, req *computepb.GetSubnetworkRequest, _ ...gax.CallOption) (*computepb.Subnetwork, error) {
	c.calls.Add(1)
	return c.get(ctx, req)
}

func freeIPs(count int64) *computepb.Subnetwork {
	return &computepb.Subnetwork{UtilizationDetails: &computepb.SubnetworkUtilizationDetails{
		Ipv4Utilizations: []*computepb.SubnetworkUtilizationDetailsIPV4Utilization{{RangeName: ptr.To("pods"), TotalFreeIp: ptr.To(count)}},
	}}
}

func TestConcurrentReadsUseCachedCapacity(t *testing.T) {
	client := &fakeSubnetworksClient{get: func(context.Context, *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
		return freeIPs(1000), nil
	}}
	p := subnet.NewProvider(client, "us-central1", clock.RealClock{})
	counts := make([]map[string]int64, 4)
	errs := make([]error, len(counts))
	var wg sync.WaitGroup
	for i := range counts {
		wg.Go(func() { counts[i], errs[i] = p.GetFreeIPCounts(t.Context(), testNetwork, "default") })
	}
	wg.Wait()
	for i := range counts {
		require.NoError(t, errs[i])
		require.EqualValues(t, 1000, counts[i]["pods"])
	}
	counts[0]["pods"] = 0
	require.EqualValues(t, 1000, counts[1]["pods"])
	cached, err := p.GetFreeIPCounts(t.Context(), testNetwork, "projects/test-project/regions/us-central1/subnetworks/default")
	require.NoError(t, err)
	require.EqualValues(t, 1000, cached["pods"])
	require.EqualValues(t, 1, client.calls.Load())
}

func TestCapacityCacheExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		available := int64(1000)
		client := &fakeSubnetworksClient{get: func(context.Context, *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
			return freeIPs(available), nil
		}}
		p := subnet.NewProvider(client, "us-central1", clock.RealClock{})
		_, err := p.GetFreeIPCounts(t.Context(), testNetwork, "default")
		require.NoError(t, err)
		available = 500
		cached, err := p.GetFreeIPCounts(t.Context(), testNetwork, "default")
		require.NoError(t, err)
		require.EqualValues(t, 1000, cached["pods"])
		time.Sleep(time.Minute)
		refreshed, err := p.GetFreeIPCounts(t.Context(), testNetwork, "default")
		require.NoError(t, err)
		require.EqualValues(t, 500, refreshed["pods"])
		require.EqualValues(t, 2, client.calls.Load())
	})
}

func TestInvalidateClearsCapacityAndErrors(t *testing.T) {
	for _, name := range []string{"capacity", "error"} {
		t.Run(name, func(t *testing.T) {
			var failure error
			if name == "error" {
				failure = errors.New("permission denied")
			}
			available := int64(1000)
			client := &fakeSubnetworksClient{get: func(context.Context, *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
				return freeIPs(available), failure
			}}
			p := subnet.NewProvider(client, "us-central1", clock.RealClock{})
			_, err := p.GetFreeIPCounts(t.Context(), testNetwork, "default")
			require.ErrorIs(t, err, failure)
			available, failure = 500, nil
			p.Invalidate(testNetwork, "default")
			counts, err := p.GetFreeIPCounts(t.Context(), testNetwork, "default")
			require.NoError(t, err)
			require.EqualValues(t, 500, counts["pods"])
			require.EqualValues(t, 2, client.calls.Load())
		})
	}
}

func TestFailedLookupRetriesAfterCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		denied := errors.New("permission denied")
		failure := denied
		client := &fakeSubnetworksClient{get: func(context.Context, *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
			return freeIPs(1000), failure
		}}
		p := subnet.NewProvider(client, "us-central1", clock.RealClock{})
		_, err := p.GetFreeIPCounts(t.Context(), testNetwork, "default")
		require.ErrorIs(t, err, denied)
		failure = nil
		counts, err := p.GetFreeIPCounts(t.Context(), testNetwork, "default")
		require.ErrorIs(t, err, denied)
		require.Nil(t, counts)
		require.EqualValues(t, 1, client.calls.Load())
		time.Sleep(10 * time.Second)
		counts, err = p.GetFreeIPCounts(t.Context(), testNetwork, "default")
		require.NoError(t, err)
		require.EqualValues(t, 1000, counts["pods"])
		require.EqualValues(t, 2, client.calls.Load())
	})
}

func TestCanceledRefreshDoesNotCacheError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &fakeSubnetworksClient{get: func(ctx context.Context, _ *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
		cancel()
		return nil, ctx.Err()
	}}
	p := subnet.NewProvider(client, "us-central1", clock.RealClock{})
	_, err := p.GetFreeIPCounts(ctx, testNetwork, "default")
	require.ErrorIs(t, err, context.Canceled)
	client.get = func(context.Context, *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
		return freeIPs(1000), nil
	}
	counts, err := p.GetFreeIPCounts(t.Context(), testNetwork, "default")
	require.NoError(t, err)
	require.EqualValues(t, 1000, counts["pods"])
	require.EqualValues(t, 2, client.calls.Load())
}

func TestCanceledCallerSkipsLookup(t *testing.T) {
	client := &fakeSubnetworksClient{get: func(context.Context, *computepb.GetSubnetworkRequest) (*computepb.Subnetwork, error) {
		return freeIPs(1000), nil
	}}
	p := subnet.NewProvider(client, "us-central1", clock.RealClock{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := p.GetFreeIPCounts(ctx, testNetwork, "default")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, client.calls.Load())
}
