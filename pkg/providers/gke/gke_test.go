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

package gke

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
	computev1 "google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	googleapioption "google.golang.org/api/option"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/operator/options"
)

func TestIsMultiNetworkingEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cluster *containerv1.Cluster
		want    bool
	}{
		{name: "no cluster"},
		{name: "no network config", cluster: &containerv1.Cluster{}},
		{name: "disabled", cluster: &containerv1.Cluster{NetworkConfig: &containerv1.NetworkConfig{}}},
		{name: "enabled", cluster: &containerv1.Cluster{NetworkConfig: &containerv1.NetworkConfig{EnableMultiNetworking: true}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, IsMultiNetworkingEnabled(tt.cluster))
		})
	}
}

func TestGetServerConfig_CacheHit(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		resp := &containerv1.ServerConfig{
			Channels: []*containerv1.ReleaseChannelConfig{
				{Channel: "STABLE", DefaultVersion: "1.34.6-gke.1068000"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	svc, err := containerv1.NewService(context.Background(),
		googleapioption.WithoutAuthentication(),
		googleapioption.WithEndpoint(srv.URL+"/"),
	)
	require.NoError(t, err)

	p := &DefaultProvider{
		containerService:  svc,
		projectID:         "p",
		nodeLocation:      "us-central1",
		serverConfigCache: cache.New(serverConfigCacheTTL, serverConfigCacheTTL),
	}

	sc1, err := p.GetServerConfig(context.Background())
	require.NoError(t, err)
	require.Len(t, sc1.Channels, 1)
	require.Equal(t, int32(1), calls.Load())

	sc2, err := p.GetServerConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, sc1, sc2)
	require.Equal(t, int32(1), calls.Load(), "second call must hit cache")
}

func TestResolveClusterZones_UsesClusterLocations(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		resp := &containerv1.Cluster{
			Name:      "test-cluster",
			Location:  "europe-west4-a",
			Locations: []string{"europe-west4-c", "europe-west4-a"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, "europe-west4-a")

	zones, err := p.ResolveClusterZones(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"europe-west4-a", "europe-west4-c"}, zones)
	require.Equal(t, int32(1), calls.Load())

	zones, err = p.ResolveClusterZones(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"europe-west4-a", "europe-west4-c"}, zones)
	require.Equal(t, int32(1), calls.Load(), "second call must hit cache")
}

func TestResolveClusterZones_FallsBackToRegionZonesWhenClusterLocationsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/projects/p/locations/europe-west4-a/clusters/test-cluster":
			_ = json.NewEncoder(w).Encode(&containerv1.Cluster{Name: "test-cluster", Location: "europe-west4-a"})
		case "/projects/p/zones":
			_ = json.NewEncoder(w).Encode(&computev1.ZoneList{Items: []*computev1.Zone{
				{Name: "europe-west4-c"},
				{Name: "europe-west4-a"},
				{Name: "us-central1-a"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, "europe-west4-a")
	ctx := options.ToContext(context.Background(), &options.Options{ProjectID: "p", ClusterLocation: "europe-west4-a"})

	zones, err := p.ResolveClusterZones(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"europe-west4-a", "europe-west4-c"}, zones)
}

func newTestProvider(t *testing.T, endpoint, nodeLocation string) *DefaultProvider {
	t.Helper()
	containerService, err := containerv1.NewService(context.Background(),
		googleapioption.WithoutAuthentication(),
		googleapioption.WithEndpoint(endpoint+"/"),
	)
	require.NoError(t, err)

	computeService, err := computev1.NewService(context.Background(),
		googleapioption.WithoutAuthentication(),
		googleapioption.WithEndpoint(endpoint+"/"),
	)
	require.NoError(t, err)

	return &DefaultProvider{
		computeService:   computeService,
		containerService: containerService,
		projectID:        "p",
		nodeLocation:     nodeLocation,
		clusterName:      "test-cluster",
		clusterCache:     cache.New(clusterCacheTTL, clusterCacheTTL),
		zoneCache:        cache.New(zoneCacheExpiration, zoneCacheCleanupInterval),
	}
}

func TestGetClusterConfig_CacheHit(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		resp := &containerv1.Cluster{
			Name:    "test-cluster",
			Network: "default",
			NetworkConfig: &containerv1.NetworkConfig{
				Network:    "projects/p/global/networks/default",
				Subnetwork: "projects/p/regions/us-central1/subnetworks/default",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	svc, err := containerv1.NewService(context.Background(),
		googleapioption.WithoutAuthentication(),
		googleapioption.WithEndpoint(srv.URL+"/"),
	)
	require.NoError(t, err)

	p := &DefaultProvider{
		containerService: svc,
		projectID:        "p",
		nodeLocation:     "us-central1",
		clusterName:      "test-cluster",
		clusterCache:     cache.New(clusterCacheTTL, clusterCacheTTL),
	}

	// First call: should hit the API.
	c1, err := p.GetClusterConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "test-cluster", c1.Name)
	require.Equal(t, int32(1), calls.Load())

	// Second call: should return from cache without calling the API.
	c2, err := p.GetClusterConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, c1, c2)
	require.Equal(t, int32(1), calls.Load(), "expected no additional API calls on cache hit")
}

func TestProjectNumber(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	numericID := uint64(123456789)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/p" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&computev1.Project{Name: "p", Id: numericID})
	}))
	defer srv.Close()

	svc, err := computev1.NewService(context.Background(),
		googleapioption.WithoutAuthentication(),
		googleapioption.WithEndpoint(srv.URL+"/"),
	)
	require.NoError(t, err)

	p := &DefaultProvider{
		computeService:  svc,
		projectID:       "p",
		projectNumCache: cache.New(0, 0),
	}

	number, err := p.ProjectNumber(context.Background())
	require.NoError(t, err)
	require.Equal(t, numericID, number)
	number, err = p.ProjectNumber(context.Background())
	require.NoError(t, err)
	require.Equal(t, numericID, number)
	require.Equal(t, int32(1), calls.Load(), "the immutable project number must be read once")

	numericID = 0
	p.projectNumCache = cache.New(0, 0)
	_, err = p.ProjectNumber(context.Background())
	require.Error(t, err, "a project without a numeric ID must not be treated as authoritative")
}

func TestShieldedNodesEnabled(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		// body is the raw cluster response. shieldedNodes: {} is how the API
		// serializes a present policy whose Enabled value is false.
		body        string
		status      int
		wantEnabled bool
		wantErr     bool
	}{
		{name: "enabled", body: `{"name":"test-cluster","shieldedNodes":{"enabled":true}}`, wantEnabled: true},
		{name: "disabled policy omitted by proto3 defaults", body: `{"name":"test-cluster","shieldedNodes":{}}`},
		{name: "explicitly disabled", body: `{"name":"test-cluster","shieldedNodes":{"enabled":false}}`},
		{name: "policy absent from the response is unknown", body: `{"name":"test-cluster"}`, wantErr: true},
		{name: "null response is unknown", body: `null`, wantErr: true},
		{name: "API failure", body: `{"error":{"code":503,"message":"unavailable"}}`, status: http.StatusServiceUnavailable, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/projects/p/locations/us-central1/clusters/test-cluster" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			enabled, err := newTestProvider(t, srv.URL, "us-central1").ShieldedNodesEnabled(context.Background())
			if tt.wantErr {
				require.Error(t, err)
				require.False(t, enabled, "an unknown policy must not look enabled or disabled")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantEnabled, enabled)
		})
	}
}

// TestShieldedNodesEnabledIgnoresTheClusterConfigCache guards the approval
// decision against the 30-minute cluster cache: an old disabled answer must not
// hide a later enabled policy or a failed read, and the read must not refill it.
func TestShieldedNodesEnabledIgnoresTheClusterConfigCache(t *testing.T) {
	var calls atomic.Int32
	responses := []func(w http.ResponseWriter){
		func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"name":"test-cluster","shieldedNodes":{"enabled":true}}`))
		},
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"name":"test-cluster","shieldedNodes":{}}`)) },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		responses[calls.Add(1)-1](w)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, "us-central1")
	// A previously cached cluster config that reports Shielded Nodes disabled.
	p.clusterCache.SetDefault(clusterCacheKey, &containerv1.Cluster{Name: "test-cluster", ShieldedNodes: &containerv1.ShieldedNodes{}})

	enabled, err := p.ShieldedNodesEnabled(context.Background())
	require.NoError(t, err)
	require.True(t, enabled, "the live policy wins over the cached disabled configuration")

	_, err = p.ShieldedNodesEnabled(context.Background())
	require.Error(t, err, "a failed read must not fall back to the cached configuration")

	enabled, err = p.ShieldedNodesEnabled(context.Background())
	require.NoError(t, err)
	require.False(t, enabled)
	require.Equal(t, int32(3), calls.Load(), "every call reads the cluster")

	cached, err := p.GetClusterConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cached.ShieldedNodes, "shared configuration discovery keeps its own cache")
	require.Equal(t, int32(3), calls.Load())
}
