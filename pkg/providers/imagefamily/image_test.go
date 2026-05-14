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

package imagefamily

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// stubGKEProvider satisfies gke.Provider for unit tests.
type stubGKEProvider struct {
	serverConfig  *containerv1.ServerConfig
	clusterConfig *containerv1.Cluster
}

func (s *stubGKEProvider) ResolveClusterZones(_ context.Context) ([]string, error) { return nil, nil }
func (s *stubGKEProvider) GetClusterConfig(_ context.Context) (*containerv1.Cluster, error) {
	return s.clusterConfig, nil
}
func (s *stubGKEProvider) GetServerConfig(_ context.Context) (*containerv1.ServerConfig, error) {
	return s.serverConfig, nil
}

// cosImageList returns a test server that always serves the given images.
func cosImageServer(t *testing.T, images []*compute.Image) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: images}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestDispatch_AliasTermResolvesImages(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z"},
	}
	srv := cosImageServer(t, images)
	defer srv.Close()

	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.1"}, &stubGKEProvider{})

	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@latest"}},
	}}

	imgs, err := p.List(context.Background(), nc)
	require.NoError(t, err)
	require.NotEmpty(t, imgs)
}

func TestDispatch_FamilyVersionLatest_ResolvesImages(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z"},
	}
	srv := cosImageServer(t, images)
	defer srv.Close()

	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.1"}, &stubGKEProvider{})

	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Version: "latest"}},
	}}

	imgs, err := p.List(context.Background(), nc)
	require.NoError(t, err)
	require.NotEmpty(t, imgs)
}

func TestDispatch_FamilyChannel_CallsGetServerConfig(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z"},
	}
	srv := cosImageServer(t, images)
	defer srv.Close()

	stub := &stubGKEProvider{
		serverConfig: &containerv1.ServerConfig{
			Channels: []*containerv1.ReleaseChannelConfig{
				{Channel: "STABLE", DefaultVersion: "1.34.6-gke.1068000"},
			},
		},
	}
	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.34.7"}, stub)

	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: v1alpha1.ImageChannelStable}},
	}}

	imgs, err := p.List(context.Background(), nc)
	require.NoError(t, err)
	require.NotEmpty(t, imgs)
}

func TestDispatch_ChannelCluster_UsesClusterChannel(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z"},
	}
	srv := cosImageServer(t, images)
	defer srv.Close()

	stub := &stubGKEProvider{
		clusterConfig: &containerv1.Cluster{
			ReleaseChannel: &containerv1.ReleaseChannel{Channel: "STABLE"},
		},
		serverConfig: &containerv1.ServerConfig{
			Channels: []*containerv1.ReleaseChannelConfig{
				{Channel: "STABLE", DefaultVersion: "1.34.6-gke.1068000"},
			},
		},
	}
	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.34.7"}, stub)

	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: v1alpha1.ImageChannelCluster}},
	}}

	imgs, err := p.List(context.Background(), nc)
	require.NoError(t, err)
	require.NotEmpty(t, imgs)
}

func TestDispatch_ChannelCluster_UnspecifiedCluster_ReturnsError(t *testing.T) {
	stub := &stubGKEProvider{
		clusterConfig: &containerv1.Cluster{
			ReleaseChannel: &containerv1.ReleaseChannel{Channel: "UNSPECIFIED"},
		},
	}
	p := NewDefaultProvider(nil, &fakeVersionProvider{version: "v1.34.7"}, stub)

	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: v1alpha1.ImageChannelCluster}},
	}}

	_, err := p.List(context.Background(), nc)
	require.Error(t, err)
	require.Contains(t, err.Error(), "UNSPECIFIED")
}
