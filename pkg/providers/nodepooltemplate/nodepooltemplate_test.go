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
	"strings"
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

// --- ensureKarpenterNodePoolTemplate tests ---

// ensureSrv builds a test server for ensureKarpenterNodePoolTemplate tests.
// clusterResp is returned for Cluster.Get; if nil, an empty cluster is returned.
// clusterStatus overrides the HTTP status for Cluster.Get (0 = 200 OK).
// createStatus overrides the HTTP status for NodePool.Create (0 = 200 OK).
// captured receives the decoded CreateNodePoolRequest when non-nil.
func ensureSrv(t *testing.T,
	clusterResp *container.Cluster,
	clusterStatus int,
	createStatus int,
	captured **container.CreateNodePoolRequest,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(path, "/nodePools/"):
			// NodePool.Get — return 404 so the creation path is exercised.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`))
		case strings.HasSuffix(path, "/nodePools") && r.Method == http.MethodPost:
			// NodePool.Create
			if captured != nil {
				var req container.CreateNodePoolRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				*captured = &req
			}
			if createStatus != 0 && createStatus != http.StatusOK {
				w.WriteHeader(createStatus)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"permission denied","status":"PERMISSION_DENIED"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(&container.Operation{Status: "RUNNING", Name: "op-1"})
		default:
			// Cluster.Get
			if clusterStatus != 0 && clusterStatus != http.StatusOK {
				w.WriteHeader(clusterStatus)
				_, _ = w.Write([]byte(`{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`))
				return
			}
			c := clusterResp
			if c == nil {
				c = &container.Cluster{}
			}
			_ = json.NewEncoder(w).Encode(c)
		}
	}))
}

func TestEnsureKarpenterNodePoolTemplate_AlwaysSetFields(t *testing.T) {
	var captured *container.CreateNodePoolRequest
	srv := ensureSrv(t, nil, 0, 0, &captured)
	defer srv.Close()

	p := provider(t, srv, "")
	err := p.ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com")
	require.NoError(t, err)
	require.NotNil(t, captured)

	cfg := captured.NodePool.Config
	require.NotNil(t, cfg.ShieldedInstanceConfig)
	require.True(t, cfg.ShieldedInstanceConfig.EnableSecureBoot)
	require.True(t, cfg.ShieldedInstanceConfig.EnableIntegrityMonitoring)
	require.NotNil(t, cfg.KubeletConfig)
	require.False(t, cfg.KubeletConfig.InsecureKubeletReadonlyPortEnabled)
	require.Equal(t, "true", cfg.Metadata["block-project-ssh-keys"])
	require.Nil(t, cfg.WorkloadMetadataConfig, "WI mode should not be set when cluster has no WI")
	require.Nil(t, captured.NodePool.NetworkConfig, "NetworkConfig should not be set for non-private cluster")
}

func TestEnsureKarpenterNodePoolTemplate_PrivateNodes_NetworkConfig(t *testing.T) {
	cluster := &container.Cluster{
		NetworkConfig: &container.NetworkConfig{DefaultEnablePrivateNodes: true},
	}
	var captured *container.CreateNodePoolRequest
	srv := ensureSrv(t, cluster, 0, 0, &captured)
	defer srv.Close()

	p := provider(t, srv, "")
	err := p.ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com")
	require.NoError(t, err)
	require.NotNil(t, captured.NodePool.NetworkConfig)
	require.True(t, captured.NodePool.NetworkConfig.EnablePrivateNodes)
}

func TestEnsureKarpenterNodePoolTemplate_PrivateNodes_LegacyField(t *testing.T) {
	cluster := &container.Cluster{
		PrivateClusterConfig: &container.PrivateClusterConfig{EnablePrivateNodes: true},
	}
	var captured *container.CreateNodePoolRequest
	srv := ensureSrv(t, cluster, 0, 0, &captured)
	defer srv.Close()

	p := provider(t, srv, "")
	err := p.ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com")
	require.NoError(t, err)
	require.NotNil(t, captured.NodePool.NetworkConfig)
	require.True(t, captured.NodePool.NetworkConfig.EnablePrivateNodes)
}

func TestEnsureKarpenterNodePoolTemplate_WorkloadIdentity(t *testing.T) {
	cluster := &container.Cluster{
		WorkloadIdentityConfig: &container.WorkloadIdentityConfig{WorkloadPool: "proj.svc.id.goog"},
	}
	var captured *container.CreateNodePoolRequest
	srv := ensureSrv(t, cluster, 0, 0, &captured)
	defer srv.Close()

	p := provider(t, srv, "")
	err := p.ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com")
	require.NoError(t, err)
	require.NotNil(t, captured.NodePool.Config.WorkloadMetadataConfig)
	require.Equal(t, "GKE_METADATA", captured.NodePool.Config.WorkloadMetadataConfig.Mode)
}

func TestEnsureKarpenterNodePoolTemplate_ClusterFetchFails_ProceedsWithDefaults(t *testing.T) {
	var captured *container.CreateNodePoolRequest
	// clusterStatus=500 simulates a transient cluster.Get failure.
	srv := ensureSrv(t, nil, http.StatusInternalServerError, 0, &captured)
	defer srv.Close()

	p := provider(t, srv, "")
	err := p.ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com")
	require.NoError(t, err, "cluster fetch failure should not abort fallback creation")
	require.NotNil(t, captured)
	// Unconditional fields must still be set.
	require.NotNil(t, captured.NodePool.Config.KubeletConfig)
	require.Equal(t, "true", captured.NodePool.Config.Metadata["block-project-ssh-keys"])
	// Conditional fields must be absent when cluster config is unavailable.
	require.Nil(t, captured.NodePool.NetworkConfig)
	require.Nil(t, captured.NodePool.Config.WorkloadMetadataConfig)
}

func TestEnsureKarpenterNodePoolTemplate_CreateError_IncludesSAName(t *testing.T) {
	// createStatus=403 simulates a service account policy violation.
	srv := ensureSrv(t, nil, 0, http.StatusForbidden, nil)
	defer srv.Close()

	p := provider(t, srv, "")
	err := p.ensureKarpenterNodePoolTemplate(context.Background(), "bad-sa@developer.gserviceaccount.com")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad-sa@developer.gserviceaccount.com", "error should name the service account")
}
