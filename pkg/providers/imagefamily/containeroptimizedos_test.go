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

package imagefamily

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

// fakeVersionProvider implements versionprovider.Provider for tests.
type fakeVersionProvider struct{ version string }

func (f *fakeVersionProvider) Get(_ context.Context) (string, error) { return f.version, nil }
func (f *fakeVersionProvider) Inject(version string)                 {}

// buildComputeService returns a compute.Service pointed at the given test server.
func buildComputeService(t *testing.T, srv *httptest.Server) *compute.Service {
	t.Helper()
	svc, err := compute.NewService(context.Background(),
		option.WithEndpoint(srv.URL+"/"),
		option.WithoutAuthentication(),
	)
	require.NoError(t, err)
	return svc
}

func TestResolveLatestCOSImage_PicksNewestNonDeprecated(t *testing.T) {
	images := []*compute.Image{
		{
			Name:              "gke-1351-gke1396004-cos-125-19216-104-126-c-pre",
			CreationTimestamp: "2025-04-01T00:00:00Z",
		},
		{
			Name:              "gke-1351-gke1390000-cos-125-19216-100-100-c-pre",
			CreationTimestamp: "2025-03-01T00:00:00Z",
		},
		{
			// deprecated — must be excluded
			Name:              "gke-1351-gke1380000-cos-125-19216-90-90-c-pre",
			CreationTimestamp: "2025-02-01T00:00:00Z",
			Deprecated:        &compute.DeprecationStatus{State: "DEPRECATED"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: images}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &ContainerOptimizedOS{
		computeService:  buildComputeService(t, srv),
		versionProvider: &fakeVersionProvider{version: "v1.35.1"},
	}

	got, err := p.resolveLatestCOSImage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre", got)
}

func TestResolveLatestCOSImage_ExcludesSpecialisedVariants(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre", CreationTimestamp: "2025-04-10T00:00:00Z"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-nvda", CreationTimestamp: "2025-04-09T00:00:00Z"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre-kmod", CreationTimestamp: "2025-04-08T00:00:00Z"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-test", CreationTimestamp: "2025-04-07T00:00:00Z"},
		{Name: "gke-1351-gke1390000-cos-125-19216-100-100-c-pre", CreationTimestamp: "2025-03-01T00:00:00Z"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: images}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &ContainerOptimizedOS{
		computeService:  buildComputeService(t, srv),
		versionProvider: &fakeVersionProvider{version: "v1.35.1"},
	}

	got, err := p.resolveLatestCOSImage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre", got)
}

func TestResolveImages_DrivesArm64AndGPUVariants(t *testing.T) {
	p := &ContainerOptimizedOS{}
	sourceImage := "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre"

	imgs := p.resolveImages(sourceImage)
	require.Len(t, imgs, 3)

	var srcs []string
	for _, img := range imgs {
		srcs = append(srcs, img.SourceImage)
	}

	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre")
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre")
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-nvda")
}

func TestBuildImageFilter_UsesVersionPrefix(t *testing.T) {
	p := &ContainerOptimizedOS{versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	got := p.buildImageFilter(context.Background())
	require.Equal(t, `name=gke-1351-*`, got)
}

func TestBuildImageFilter_FallsBackOnNilProvider(t *testing.T) {
	p := &ContainerOptimizedOS{}
	got := p.buildImageFilter(context.Background())
	require.Equal(t, `name=gke-*-cos-*-c-pre`, got)
}

func TestRenderVersion(t *testing.T) {
	require.Equal(t, "1-35-1", renderVersion("v1.35.1"))
	require.Equal(t, "1-35-1", renderVersion("1.35.1"))
}
