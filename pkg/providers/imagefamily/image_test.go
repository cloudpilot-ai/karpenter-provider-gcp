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
	"path"
	"strconv"
	"strings"
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

func readyTestImages(images []*compute.Image) []*compute.Image {
	for _, img := range images {
		if img.Status == "" {
			img.Status = "READY"
		}
	}
	return images
}

// imageCatalog is a mutable image catalog served by imageCatalogServer. Pages are served in the
// given order; a catalog is ordered newest first, as images.list orders it with
// creationTimestamp desc.
type imageCatalog struct {
	pages  [][]*compute.Image
	lookup func(name string) (*compute.Image, int)
}

// imageCatalogServer serves catalog listing pages and per-image lookups, including pagination.
// A lookup override answers one image name first: a non-zero status writes that HTTP status, a
// non-nil image is served instead of the catalog copy, and (nil, 0) falls back to the catalog.
// Overrides model a candidate that disappears, changes status, or fails after being listed.
func imageCatalogServer(t *testing.T, catalog *imageCatalog) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/images") {
			page := 0
			if token := r.URL.Query().Get("pageToken"); token != "" {
				page, _ = strconv.Atoi(token)
			}
			list := &compute.ImageList{}
			if page < len(catalog.pages) {
				list.Items = catalog.pages[page]
				if page+1 < len(catalog.pages) {
					list.NextPageToken = strconv.Itoa(page + 1)
				}
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		name := path.Base(r.URL.Path)
		if catalog.lookup != nil {
			if img, status := catalog.lookup(name); status != 0 {
				http.Error(w, http.StatusText(status), status)
				return
			} else if img != nil {
				_ = json.NewEncoder(w).Encode(img)
				return
			}
		}
		for _, page := range catalog.pages {
			for _, img := range page {
				if img.Name == name {
					_ = json.NewEncoder(w).Encode(img)
					return
				}
			}
		}
		http.Error(w, "image not found", http.StatusNotFound)
	}))
}

// cosImageServer serves a fixed single-page COS catalog and the images it lists.
func cosImageServer(t *testing.T, images []*compute.Image) *httptest.Server {
	t.Helper()
	return imageCatalogServer(t, &imageCatalog{pages: [][]*compute.Image{images}})
}

// cosLatestNodeClass selects the newest available COS images.
func cosLatestNodeClass() *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@latest"}},
	}}
}

func TestResolveIDTerm_RejectsPendingImage(t *testing.T) {
	name := "projects/gke-node-images/global/images/test-image"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&compute.Image{Name: "test-image", Status: "PENDING", Architecture: OSArchitectureX86})
	}))
	defer srv.Close()
	p := NewDefaultProvider(buildComputeService(t, srv), nil, nil)
	_, err := p.List(context.Background(), &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{ID: name}},
	}})
	require.ErrorContains(t, err, "PENDING")
	require.False(t, IsImageResolutionError(err))
	require.True(t, IsImageUnavailableError(err), "a pending publication is not an infrastructure failure")
}

func TestList_PublicationRefreshesWithoutImageHistory(t *testing.T) {
	const (
		amd64    = "gke-1351-gke1396004-cos-125-19216-104-126-c-pre"
		newArm64 = "gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre"
		oldArm64 = "gke-1351-gke1390000-cos-arm64-125-19216-100-100-c-pre"
		prefix   = "projects/gke-node-images/global/images/"
	)
	catalog := &imageCatalog{}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()
	service := buildComputeService(t, srv)
	newProvider := func() *DefaultProvider {
		return NewDefaultProvider(service, &fakeVersionProvider{version: "v1.35.1"}, nil)
	}
	p := newProvider()
	nc := cosLatestNodeClass()
	_, err := p.List(context.Background(), nc)
	require.True(t, IsImageUnavailableError(err))

	var previous Images
	for _, phase := range []struct {
		name    string
		arm64   []*compute.Image
		wantArm string
	}{
		{"missing newest", []*compute.Image{{Name: oldArm64, Status: "READY"}}, oldArm64},
		{"pending newest", []*compute.Image{{Name: newArm64, Status: "PENDING"}, {Name: oldArm64, Status: "READY"}}, oldArm64},
		{"ready newest", []*compute.Image{{Name: newArm64, Status: "READY"}, {Name: oldArm64, Status: "READY"}}, newArm64},
		{"deprecated newest", []*compute.Image{{Name: newArm64, Status: "READY", Deprecated: &compute.DeprecationStatus{State: "DEPRECATED"}}, {Name: oldArm64, Status: "READY"}}, oldArm64},
		{"fallback deleted", nil, ""},
	} {
		t.Run(phase.name, func(t *testing.T) {
			catalog.pages = [][]*compute.Image{append([]*compute.Image{{Name: amd64, Status: "READY"}}, phase.arm64...)}
			if previous != nil {
				cached, err := p.List(context.Background(), cosLatestNodeClass())
				require.NoError(t, err)
				require.Equal(t, imageSources(previous), imageSources(cached), "new NodeClasses share the selector cache until refresh")
				p.cache.Flush() // Force a refresh without sleeping for the five-minute TTL.
			}
			images, err := p.List(context.Background(), nc)
			require.NoError(t, err, "the initial unavailable result must not be cached")
			want := []string{prefix + amd64}
			if phase.wantArm != "" {
				want = append(want, prefix+phase.wantArm)
			}
			require.Equal(t, want, imageSources(images))
			fresh, err := newProvider().List(context.Background(), cosLatestNodeClass())
			require.NoError(t, err)
			require.Equal(t, want, imageSources(fresh), "a fresh controller must discover the same fallback")
			previous = images
		})
	}
}

func TestList_SelectorChangeDoesNotReuseCachedImages(t *testing.T) {
	const sideImage = "test-image"
	catalog := &imageCatalog{pages: [][]*compute.Image{{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", Status: "READY"},
		{Name: sideImage, Status: "READY", Architecture: OSArchitectureX86},
	}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.1"}, &stubGKEProvider{})
	_, err := p.List(context.Background(), cosLatestNodeClass())
	require.NoError(t, err)

	byID := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{ID: "projects/gke-node-images/global/images/" + sideImage}},
	}}
	images, err := p.List(context.Background(), byID)
	require.NoError(t, err)
	require.Equal(t, []string{"projects/gke-node-images/global/images/" + sideImage}, imageSources(images))
}

func TestDispatch_AliasTermResolvesImages(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z", Status: "READY"},
		{Name: "gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre", Status: "READY"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-nvda", Status: "READY"},
	}
	srv := cosImageServer(t, images)
	defer srv.Close()

	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.1"}, &stubGKEProvider{})

	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@latest"}},
	}}

	imgs, err := p.List(context.Background(), nc)
	require.NoError(t, err)
	require.Len(t, imgs, 3)
}

func TestDispatch_FamilyVersionLatest_ResolvesImages(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z", Status: "READY"},
		{Name: "gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre", Status: "READY"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-nvda", Status: "READY"},
	}
	srv := cosImageServer(t, images)
	defer srv.Close()

	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.1"}, &stubGKEProvider{})

	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Version: "latest"}},
	}}

	imgs, err := p.List(context.Background(), nc)
	require.NoError(t, err)
	require.Len(t, imgs, 3)
}

func TestDispatch_FamilyChannel_CallsGetServerConfig(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z", Status: "READY"},
		{Name: "gke-1346-gke1068000-cos-arm64-125-19216-220-72-c-pre", Status: "READY"},
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-nvda", Status: "READY"},
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
	require.Len(t, imgs, 3)
}

func TestDispatch_ChannelCluster_UsesClusterChannel(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z", Status: "READY"},
		{Name: "gke-1346-gke1068000-cos-arm64-125-19216-220-72-c-pre", Status: "READY"},
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-nvda", Status: "READY"},
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
	require.Len(t, imgs, 3)
}

func TestDispatch_FamilyChannel_OmitsUnpublishedSiblingsWithoutSubstitution(t *testing.T) {
	images := []*compute.Image{
		// Exact COS build resolved from the channel.
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", Status: "READY"},
		// Newer COS image for a different build: it must never be substituted for the exact one.
		{Name: "gke-1346-gke1069000-cos-125-19216-221-100-c-pre", Status: "READY"},
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
	require.Equal(t, []string{"projects/gke-node-images/global/images/gke-1346-gke1068000-cos-125-19216-220-72-c-pre"}, imageSources(imgs))
}

func TestCOSExactSelectorsWithoutUsableImagesAreUnavailable(t *testing.T) {
	const base = "gke-1346-gke1068000-cos-125-19216-220-72-c-pre"
	for _, tc := range []struct {
		name, version string
		images        []*compute.Image
		missingOnGet  bool
	}{
		{"absent exact build", "1.34.6-gke.1068000", nil, false},
		{"pending exact build", "1.34.6-gke.1068000", []*compute.Image{{Name: base, Status: "PENDING"}}, false},
		{"absent milestone base", "125.19216.104.126", nil, false},
		{"all derived images vanished", "1.34.6-gke.1068000", []*compute.Image{{Name: base, Status: "READY"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &imageCatalog{pages: [][]*compute.Image{tc.images}}
			if tc.missingOnGet {
				catalog.lookup = func(string) (*compute.Image, int) { return nil, http.StatusNotFound }
			}
			srv := imageCatalogServer(t, catalog)
			t.Cleanup(srv.Close)
			p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.1"}, nil)
			nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
				ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@" + tc.version}},
			}}
			images, err := p.List(context.Background(), nc)
			require.True(t, IsImageUnavailableError(err), "expected publication unavailability, got %v", err)
			require.Empty(t, images)
		})
	}
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

func (*stubGKEProvider) InvalidateClusterConfig() {}
