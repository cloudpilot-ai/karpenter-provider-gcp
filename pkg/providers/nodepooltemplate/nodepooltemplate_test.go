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

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/option"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/metadata"
)

func buildContainerService(t *testing.T, srv *httptest.Server) *container.Service {
	t.Helper()
	svc, err := container.NewService(context.Background(),
		option.WithEndpoint(srv.URL+"/"),
		option.WithoutAuthentication(),
	)
	require.NoError(t, err)
	return svc
}

func buildComputeService(t *testing.T, srv *httptest.Server) *compute.Service {
	t.Helper()
	svc, err := compute.NewService(context.Background(),
		option.WithEndpoint(srv.URL+"/"),
		option.WithoutAuthentication(),
	)
	require.NoError(t, err)
	return svc
}

func nodePoolsHandler(pools []*container.NodePool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&container.ListNodePoolsResponse{NodePools: pools})
	}
}

func singlePoolHandler(p *container.NodePool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	}
}

func newProvider(t *testing.T, srv *httptest.Server, preferred string) *DefaultProvider {
	t.Helper()
	return &DefaultProvider{
		containerService:  buildContainerService(t, srv),
		preferredPoolName: preferred,
		ClusterInfo:       ClusterInfo{ProjectID: "proj", NodeLocation: "us-central1", Name: "cluster"},
	}
}

func pool(name, status string) *container.NodePool {
	return &container.NodePool{Name: name, Status: status}
}

// --- discoverSourcePool ---

func TestDiscoverSourcePool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		pools     []*container.NodePool // nil → use singlePoolHandler with preferred
		preferred string
		want      string
		wantErr   string
	}{
		{
			name:      "preferred pool returned directly",
			preferred: "pinned-pool",
			want:      "pinned-pool",
		},
		{
			name:      "preferred pool not running",
			preferred: "pinned-pool",
			wantErr:   "must be RUNNING or RUNNING_WITH_ERROR",
		},
		{
			name:  "default-pool preferred over alphabetical first",
			pools: []*container.NodePool{pool("other-pool", "RUNNING"), pool("default-pool", "RUNNING"), pool("alpha-pool", "RUNNING")},
			want:  "default-pool",
		},
		{
			name:  "first alphabetically when no default-pool",
			pools: []*container.NodePool{pool("zoo-pool", "RUNNING"), pool("alpha-pool", "RUNNING"), pool("mid-pool", "RUNNING")},
			want:  "alpha-pool",
		},
		{
			name:  "skips non-running pools",
			pools: []*container.NodePool{pool("zzz-pool", "PROVISIONING"), pool("default-pool", "RECONCILING"), pool("good-pool", "RUNNING")},
			want:  "good-pool",
		},
		{
			name:  "RUNNING_WITH_ERROR is eligible",
			pools: []*container.NodePool{pool("default-pool", "RUNNING_WITH_ERROR")},
			want:  "default-pool",
		},
		{
			name:    "no pools returns error",
			pools:   nil,
			wantErr: "no eligible",
		},
		{
			name:    "all non-running returns error",
			pools:   []*container.NodePool{pool("default-pool", "PROVISIONING"), pool("other-pool", "RECONCILING")},
			wantErr: "",
		},
		{
			name:  "karpenter-prefixed pool eligible",
			pools: []*container.NodePool{pool("karpenter-workload", "RUNNING")},
			want:  "karpenter-workload",
		},
		{
			name: "karpenter-fallback sorted after user pools",
			pools: []*container.NodePool{
				pool(KarpenterFallbackNodePoolTemplate, "RUNNING"),
				pool("zoo-pool", "RUNNING"),
				pool("alpha-pool", "RUNNING"),
			},
			want: "alpha-pool",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var srv *httptest.Server
			if tc.preferred != "" {
				status := "RUNNING"
				if tc.wantErr != "" {
					status = "PROVISIONING"
				}
				srv = httptest.NewServer(singlePoolHandler(pool(tc.preferred, status)))
			} else {
				srv = httptest.NewServer(nodePoolsHandler(tc.pools))
			}
			defer srv.Close()

			p := newProvider(t, srv, tc.preferred)
			got, err := p.discoverSourcePool(context.Background())
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			if tc.want == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestIsPoolEligible(t *testing.T) {
	for _, s := range []string{"RUNNING", "RUNNING_WITH_ERROR"} {
		require.True(t, isPoolEligible(s), "expected %q eligible", s)
	}
	for _, s := range []string{"PROVISIONING", "RECONCILING", "STOPPING", "ERROR", "STATUS_UNSPECIFIED", ""} {
		require.False(t, isPoolEligible(s), "expected %q ineligible", s)
	}
}

func TestGetSourcePoolNameInternal(t *testing.T) {
	t.Run("before discovery", func(t *testing.T) {
		_, err := (&DefaultProvider{}).getSourcePoolName(context.Background())
		require.ErrorContains(t, err, "not yet discovered")
	})
	t.Run("after discovery", func(t *testing.T) {
		name, err := (&DefaultProvider{sourcePoolName: "default-pool"}).getSourcePoolName(context.Background())
		require.NoError(t, err)
		require.Equal(t, "default-pool", name)
	})
}

func TestGetSourceTemplateMetadata(t *testing.T) {
	t.Run("returns metadata from matching source pool template", func(t *testing.T) {
		template := instanceTemplate("template", "cluster", "default-pool")
		srv := sourceTemplateServer(t, []*compute.InstanceTemplate{template})
		defer srv.Close()

		p := sourceTemplateProvider(t, srv)
		source, err := p.GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		require.Equal(t, "cluster", source[metadata.ClusterNameLabel])
	})

	t.Run("ignores templates from other clusters", func(t *testing.T) {
		srv := sourceTemplateServer(t, []*compute.InstanceTemplate{
			instanceTemplate("other-cluster", "other", "default-pool"),
			instanceTemplate("target", "cluster", "default-pool"),
		})
		defer srv.Close()

		source, err := sourceTemplateProvider(t, srv).GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		require.Equal(t, "cluster", source[metadata.ClusterNameLabel])
	})

	t.Run("missing matching template returns error", func(t *testing.T) {
		srv := sourceTemplateServer(t, []*compute.InstanceTemplate{instanceTemplate("template", "cluster", "other-pool")})
		defer srv.Close()

		_, err := sourceTemplateProvider(t, srv).GetSourceTemplateMetadata(context.Background())
		require.ErrorContains(t, err, "no instance template found")
	})

	t.Run("returned metadata items are copied", func(t *testing.T) {
		template := instanceTemplate("template", "cluster", "default-pool")
		srv := sourceTemplateServer(t, []*compute.InstanceTemplate{template})
		defer srv.Close()

		source, err := sourceTemplateProvider(t, srv).GetSourceTemplateMetadata(context.Background())
		require.NoError(t, err)
		source["new"] = "value"

		require.NotContains(t, metadata.FromAPI(template.Properties.Metadata), "new")
	})
}

func sourceTemplateProvider(t *testing.T, srv *httptest.Server) *DefaultProvider {
	t.Helper()
	return &DefaultProvider{
		computeService: buildComputeService(t, srv),
		sourcePoolName: "default-pool",
		ClusterInfo: ClusterInfo{
			ProjectID: "proj",
			Region:    "us-central1",
			Name:      "cluster",
		},
	}
}

func sourceTemplateServer(t *testing.T, templates []*compute.InstanceTemplate) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/regions/us-central1/instanceTemplates") {
			_ = json.NewEncoder(w).Encode(&compute.InstanceTemplateList{Items: templates})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func instanceTemplate(name, clusterName, nodePoolName string) *compute.InstanceTemplate {
	return &compute.InstanceTemplate{
		Name: name,
		Properties: &compute.InstanceProperties{
			Labels:   map[string]string{"goog-k8s-node-pool-name": nodePoolName},
			Metadata: &compute.Metadata{Items: []*compute.MetadataItems{{Key: "cluster-name", Value: lo.ToPtr(clusterName)}}},
		},
	}
}

// --- ensureKarpenterNodePoolTemplate ---

// ensureSrv builds a test server for ensureKarpenterNodePoolTemplate tests.
// clusterStatus/createStatus of 0 means HTTP 200. captured receives the decoded request.
func ensureSrv(t *testing.T, clusterResp *container.Cluster, clusterStatus, createStatus int, captured **container.CreateNodePoolRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(path, "/nodePools/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`))
		case strings.HasSuffix(path, "/nodePools") && r.Method == http.MethodPost:
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

	require.NoError(t, newProvider(t, srv, "").ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com"))
	require.NotNil(t, captured)

	cfg := captured.NodePool.Config
	require.True(t, cfg.ShieldedInstanceConfig.EnableSecureBoot)
	require.True(t, cfg.ShieldedInstanceConfig.EnableIntegrityMonitoring)
	require.False(t, cfg.KubeletConfig.InsecureKubeletReadonlyPortEnabled)
	require.Equal(t, "true", cfg.Metadata["block-project-ssh-keys"])
	require.Nil(t, cfg.WorkloadMetadataConfig, "no WI when cluster has none")
	require.Nil(t, captured.NodePool.NetworkConfig, "no NetworkConfig for non-private cluster")
}

func TestEnsureKarpenterNodePoolTemplate_PrivateNodes(t *testing.T) {
	cases := []struct {
		name    string
		cluster *container.Cluster
	}{
		{"DefaultEnablePrivateNodes", &container.Cluster{NetworkConfig: &container.NetworkConfig{DefaultEnablePrivateNodes: true}}},
		{"PrivateClusterConfig", &container.Cluster{PrivateClusterConfig: &container.PrivateClusterConfig{EnablePrivateNodes: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured *container.CreateNodePoolRequest
			srv := ensureSrv(t, tc.cluster, 0, 0, &captured)
			defer srv.Close()
			require.NoError(t, newProvider(t, srv, "").ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com"))
			require.NotNil(t, captured.NodePool.NetworkConfig)
			require.True(t, captured.NodePool.NetworkConfig.EnablePrivateNodes)
		})
	}
}

func TestEnsureKarpenterNodePoolTemplate_WorkloadIdentity(t *testing.T) {
	cluster := &container.Cluster{WorkloadIdentityConfig: &container.WorkloadIdentityConfig{WorkloadPool: "proj.svc.id.goog"}}
	var captured *container.CreateNodePoolRequest
	srv := ensureSrv(t, cluster, 0, 0, &captured)
	defer srv.Close()

	require.NoError(t, newProvider(t, srv, "").ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com"))
	require.Equal(t, "GKE_METADATA", captured.NodePool.Config.WorkloadMetadataConfig.Mode)
}

func TestEnsureKarpenterNodePoolTemplate_ClusterFetchFails_ProceedsWithDefaults(t *testing.T) {
	var captured *container.CreateNodePoolRequest
	srv := ensureSrv(t, nil, http.StatusInternalServerError, 0, &captured)
	defer srv.Close()

	require.NoError(t, newProvider(t, srv, "").ensureKarpenterNodePoolTemplate(context.Background(), "sa@proj.iam.gserviceaccount.com"),
		"cluster fetch failure should not abort fallback creation")
	require.NotNil(t, captured.NodePool.Config.KubeletConfig)
	require.Equal(t, "true", captured.NodePool.Config.Metadata["block-project-ssh-keys"])
	require.Nil(t, captured.NodePool.NetworkConfig, "no conditional fields when cluster config unavailable")
	require.Nil(t, captured.NodePool.Config.WorkloadMetadataConfig)
}

func TestEnsureKarpenterNodePoolTemplate_CreateError_IncludesSAName(t *testing.T) {
	srv := ensureSrv(t, nil, 0, http.StatusForbidden, nil)
	defer srv.Close()

	err := newProvider(t, srv, "").ensureKarpenterNodePoolTemplate(context.Background(), "bad-sa@developer.gserviceaccount.com")
	require.ErrorContains(t, err, "bad-sa@developer.gserviceaccount.com")
}
