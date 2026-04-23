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
	containerv1 "google.golang.org/api/container/v1"
	"google.golang.org/api/option"

	"github.com/stretchr/testify/require"
)

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
		option.WithoutAuthentication(),
		option.WithEndpoint(srv.URL+"/"),
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
