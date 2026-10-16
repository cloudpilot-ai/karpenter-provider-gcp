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

package serviceaccount_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	"k8s.io/utils/clock"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/serviceaccount"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time                  { return c.now }
func (c *testClock) Since(t time.Time) time.Duration { return c.now.Sub(t) }

func TestStartupFailsOnDiscoveryError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "discovery unavailable", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	client, err := compute.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	_, err = serviceaccount.NewProvider(context.Background(), client, "project", "", clock.RealClock{})
	require.ErrorContains(t, err, "getting Compute Engine default service account")
}

func TestEmptyDefaultRecovers(t *testing.T) {
	t.Parallel()
	account := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"defaultServiceAccount":%q}`, account)
	}))
	t.Cleanup(server.Close)
	client, err := compute.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	clock := &testClock{now: time.Now()}
	provider, err := serviceaccount.NewProvider(context.Background(), client, "project", "", clock)
	require.NoError(t, err, "a successful empty lookup must not prevent startup")
	resolved, err := provider.Resolve(context.Background(), "")
	require.NoError(t, err)
	require.Empty(t, resolved)
	account = "default@example.com"
	clock.now = clock.now.Add(time.Minute)
	resolved, err = provider.Resolve(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, account, resolved)
}

func TestDiscoveryFailuresAreThrottledAndRecover(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	var phase atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		switch phase.Load() {
		case 0:
			_, _ = w.Write([]byte(`{}`))
		case 1:
			http.Error(w, "discovery unavailable", http.StatusForbidden)
		default:
			_, _ = w.Write([]byte(`{"defaultServiceAccount":"default@example.com"}`))
		}
	}))
	t.Cleanup(server.Close)
	client, err := compute.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	clock := &testClock{now: time.Now()}
	provider, err := serviceaccount.NewProvider(context.Background(), client, "project", "", clock)
	require.NoError(t, err)

	// Concurrent NodeClasses share a cooldown, including empty successful responses.
	checkConcurrent := func(wantError bool) {
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				account, resolveErr := provider.Resolve(context.Background(), "")
				if account != "" || (resolveErr != nil) != wantError {
					t.Errorf("unexpected resolution: %q, %v", account, resolveErr)
				}
			})
		}
		wg.Wait()
	}
	checkConcurrent(false)
	require.EqualValues(t, 1, requests.Load())
	phase.Store(1)
	clock.now = clock.now.Add(time.Minute)
	checkConcurrent(true)
	require.EqualValues(t, 2, requests.Load())

	// An explicit NodeClass account bypasses a failed project-default lookup.
	account, err := provider.Resolve(context.Background(), "explicit@example.com")
	require.NoError(t, err)
	require.Equal(t, "explicit@example.com", account)
	phase.Store(2)
	clock.now = clock.now.Add(time.Minute)
	account, err = provider.Resolve(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "default@example.com", account)
	phase.Store(1)
	clock.now = clock.now.Add(time.Hour)
	account, err = provider.Resolve(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "default@example.com", account)
	require.EqualValues(t, 3, requests.Load(), "successful resolution remains cached")
}

func TestConfiguredAccountSkipsDiscovery(t *testing.T) {
	t.Parallel()
	// No Compute client is needed when the controller account is configured.
	provider, err := serviceaccount.NewProvider(context.Background(), nil, "project", "controller@example.com", clock.RealClock{})
	require.NoError(t, err)
	account, err := provider.Resolve(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "controller@example.com", account)
	account, err = provider.Resolve(context.Background(), "nodeclass@example.com")
	require.NoError(t, err)
	require.Equal(t, "nodeclass@example.com", account)
}
