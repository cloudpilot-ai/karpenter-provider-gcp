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
	containerv1 "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
)

func makeServerConfig(channels ...*containerv1.ReleaseChannelConfig) *containerv1.ServerConfig {
	return &containerv1.ServerConfig{Channels: channels}
}

func TestResolveVersionForChannel_DefaultVersionMinorMatch(t *testing.T) {
	sc := makeServerConfig(&containerv1.ReleaseChannelConfig{
		Channel:        "STABLE",
		DefaultVersion: "1.34.6-gke.1068000",
		ValidVersions:  []string{"1.34.6-gke.1068000", "1.33.5-gke.900000"},
	})
	got, err := ResolveVersionForChannel(sc, "stable", "1.34.7")
	require.NoError(t, err)
	require.Equal(t, "1.34.6-gke.1068000", got)
}

func TestResolveVersionForChannel_NormalisesChannelNameToUppercase(t *testing.T) {
	sc := makeServerConfig(&containerv1.ReleaseChannelConfig{
		Channel:        "RAPID",
		DefaultVersion: "1.35.3-gke.1737000",
		ValidVersions:  []string{"1.35.3-gke.1737000"},
	})
	got, err := ResolveVersionForChannel(sc, "RAPID", "1.35.1")
	require.NoError(t, err)
	require.Equal(t, "1.35.3-gke.1737000", got)
}

func TestResolveVersionForChannel_FallsBackToValidVersions(t *testing.T) {
	// defaultVersion minor (1.35) doesn't match cluster minor (1.34); validVersions has two 1.34 entries.
	sc := makeServerConfig(&containerv1.ReleaseChannelConfig{
		Channel:        "STABLE",
		DefaultVersion: "1.35.3-gke.1389000",
		ValidVersions:  []string{"1.35.3-gke.1389000", "1.34.6-gke.1068000", "1.34.5-gke.900000"},
	})
	got, err := ResolveVersionForChannel(sc, "stable", "1.34.7")
	require.NoError(t, err)
	require.Equal(t, "1.34.6-gke.1068000", got)
}

func TestResolveVersionForChannel_SemverOrderNotLexicographic(t *testing.T) {
	// 1.34.10 > 1.34.9 semver, but "1.34.10" < "1.34.9" lexicographically.
	sc := makeServerConfig(&containerv1.ReleaseChannelConfig{
		Channel:        "REGULAR",
		DefaultVersion: "1.35.0-gke.100000",
		ValidVersions:  []string{"1.34.9-gke.900000", "1.34.10-gke.1000000"},
	})
	got, err := ResolveVersionForChannel(sc, "regular", "1.34.5")
	require.NoError(t, err)
	require.Equal(t, "1.34.10-gke.1000000", got)
}

func TestResolveVersionForChannel_NoMinorMatch(t *testing.T) {
	sc := makeServerConfig(&containerv1.ReleaseChannelConfig{
		Channel:        "STABLE",
		DefaultVersion: "1.35.3-gke.1389000",
		ValidVersions:  []string{"1.35.3-gke.1389000"},
	})
	_, err := ResolveVersionForChannel(sc, "stable", "1.34.7")
	require.Error(t, err)
	require.Contains(t, err.Error(), "channel STABLE has no valid version for cluster minor")
}

func TestResolveVersionForChannel_ChannelNotFound(t *testing.T) {
	sc := makeServerConfig()
	_, err := ResolveVersionForChannel(sc, "stable", "1.34.7")
	require.Error(t, err)
	require.Contains(t, err.Error(), "channel STABLE not found in getServerConfig response")
}

func TestResolveVersionForChannel_EmptyDefaultVersionFallsToValidVersions(t *testing.T) {
	sc := makeServerConfig(&containerv1.ReleaseChannelConfig{
		Channel:        "EXTENDED",
		DefaultVersion: "",
		ValidVersions:  []string{"1.34.6-gke.1068000"},
	})
	got, err := ResolveVersionForChannel(sc, "extended", "1.34.0")
	require.NoError(t, err)
	require.Equal(t, "1.34.6-gke.1068000", got)
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
		option.WithoutAuthentication(),
		option.WithEndpoint(srv.URL+"/"),
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
