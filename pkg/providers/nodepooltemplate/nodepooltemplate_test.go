/*
Copyright 2025 The CloudPilot AI Authors.

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

package nodepooltemplate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/option"
)

// buildContainerService creates a container.Service pointed at the given test server.
func buildContainerService(t *testing.T, srv *httptest.Server) *container.Service {
	t.Helper()
	svc, err := container.NewService(context.Background(),
		option.WithEndpoint(srv.URL+"/"),
		option.WithoutAuthentication(),
	)
	require.NoError(t, err)
	return svc
}

// nodePoolsHandler returns a handler that responds to NodePools.List with the supplied pools.
func nodePoolsHandler(pools []*container.NodePool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := &container.ListNodePoolsResponse{NodePools: pools}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func provider(t *testing.T, srv *httptest.Server, preferred string) *DefaultProvider {
	t.Helper()
	return &DefaultProvider{
		containerService:  buildContainerService(t, srv),
		preferredPoolName: preferred,
		ClusterInfo: ClusterInfo{
			ProjectID:    "proj",
			NodeLocation: "us-central1",
			Name:         "cluster",
		},
	}
}

func pool(name, status string) *container.NodePool {
	return &container.NodePool{Name: name, Status: status}
}

func TestDiscoverSourcePool_PreferredPool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// NodePools.Get is a GET request ending in /nodePools/{name}
		p := pool("pinned-pool", "RUNNING")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	}))
	defer srv.Close()

	p := provider(t, srv, "pinned-pool")
	selected, err := p.discoverSourcePool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "pinned-pool", selected)
}

func TestDiscoverSourcePool_PreferredPool_NotRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := pool("pinned-pool", "PROVISIONING")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	}))
	defer srv.Close()

	p := provider(t, srv, "pinned-pool")
	_, err := p.discoverSourcePool(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not RUNNING")
}

func TestDiscoverSourcePool_DefaultPool(t *testing.T) {
	pools := []*container.NodePool{
		pool("other-pool", "RUNNING"),
		pool("default-pool", "RUNNING"),
		pool("another-pool", "RUNNING"),
	}
	srv := httptest.NewServer(nodePoolsHandler(pools))
	defer srv.Close()

	p := provider(t, srv, "")
	selected, err := p.discoverSourcePool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "default-pool", selected, "default-pool should be preferred")
}

func TestDiscoverSourcePool_NoDefaultPool_AlphaFirst(t *testing.T) {
	pools := []*container.NodePool{
		pool("zoo-pool", "RUNNING"),
		pool("alpha-pool", "RUNNING"),
		pool("mid-pool", "RUNNING"),
	}
	srv := httptest.NewServer(nodePoolsHandler(pools))
	defer srv.Close()

	p := provider(t, srv, "")
	selected, err := p.discoverSourcePool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "alpha-pool", selected, "first pool alphabetically should be selected")
}

func TestDiscoverSourcePool_SkipsNonRunning(t *testing.T) {
	pools := []*container.NodePool{
		pool("zzz-pool", "PROVISIONING"),
		pool("default-pool", "RECONCILING"),
		pool("good-pool", "RUNNING"),
	}
	srv := httptest.NewServer(nodePoolsHandler(pools))
	defer srv.Close()

	p := provider(t, srv, "")
	selected, err := p.discoverSourcePool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "good-pool", selected)
}

func TestDiscoverSourcePool_RunningWithError_Eligible(t *testing.T) {
	pools := []*container.NodePool{
		pool("default-pool", "RUNNING_WITH_ERROR"),
	}
	srv := httptest.NewServer(nodePoolsHandler(pools))
	defer srv.Close()

	p := provider(t, srv, "")
	selected, err := p.discoverSourcePool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "default-pool", selected)
}

func TestDiscoverSourcePool_NoPools(t *testing.T) {
	srv := httptest.NewServer(nodePoolsHandler(nil))
	defer srv.Close()

	p := provider(t, srv, "")
	_, err := p.discoverSourcePool(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no RUNNING")
}

func TestDiscoverSourcePool_AllProvisioning(t *testing.T) {
	pools := []*container.NodePool{
		pool("default-pool", "PROVISIONING"),
		pool("other-pool", "RECONCILING"),
	}
	srv := httptest.NewServer(nodePoolsHandler(pools))
	defer srv.Close()

	p := provider(t, srv, "")
	_, err := p.discoverSourcePool(context.Background())
	require.Error(t, err)
}

func TestDiscoverSourcePool_LegacyPoolsLogged(t *testing.T) {
	// Ensure legacy pool names don't interfere with selection.
	pools := []*container.NodePool{
		pool(KarpenterUbuntuNodePoolTemplate, "RUNNING"),
		pool(KarpenterCOSARM64NodePoolTemplate, "RUNNING"),
		pool("default-pool", "RUNNING"),
	}
	srv := httptest.NewServer(nodePoolsHandler(pools))
	defer srv.Close()

	p := provider(t, srv, "")
	selected, err := p.discoverSourcePool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "default-pool", selected, "default-pool should still win")
}

func TestIsPoolEligible(t *testing.T) {
	eligible := []string{"RUNNING", "RUNNING_WITH_ERROR"}
	ineligible := []string{"PROVISIONING", "RECONCILING", "STOPPING", "ERROR", "STATUS_UNSPECIFIED", ""}

	for _, s := range eligible {
		require.True(t, isPoolEligible(s), "expected %q to be eligible", s)
	}
	for _, s := range ineligible {
		require.False(t, isPoolEligible(s), "expected %q to be ineligible", s)
	}
}

func TestGetSourcePoolName_BeforeDiscovery(t *testing.T) {
	p := &DefaultProvider{}
	_, err := p.GetSourcePoolName(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not yet discovered")
}

func TestGetSourcePoolName_AfterDiscovery(t *testing.T) {
	p := &DefaultProvider{sourcePoolName: "default-pool"}
	name, err := p.GetSourcePoolName(context.Background())
	require.NoError(t, err)
	require.Equal(t, "default-pool", name)
}

// Ensure no regression: non-legacy pool names that happen to start with "karpenter" are
// not treated as legacy pools.
func TestDiscoverSourcePool_NonLegacyKarpenterPool(t *testing.T) {
	pools := []*container.NodePool{
		pool("karpenter-workload", "RUNNING"),
	}
	srv := httptest.NewServer(nodePoolsHandler(pools))
	defer srv.Close()

	p := provider(t, srv, "")
	selected, err := p.discoverSourcePool(context.Background())
	require.NoError(t, err)
	require.Equal(t, "karpenter-workload", selected)
}
