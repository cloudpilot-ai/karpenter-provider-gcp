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
	v1 "k8s.io/api/core/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
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

func TestClassifyCOSImage(t *testing.T) {
	cases := []struct {
		name string
		want cosVariant
		ok   bool
	}{
		{"gke-1351-gke1396004-cos-125-19216-104-126-c-pre", cosVariantAMD64CPU, true},
		{"gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre", cosVariantARM64CPU, true},
		{"gke-1351-gke1396004-cos-125-19216-104-126-c-nvda", cosVariantAMD64NVIDIA, true},
		// Specialised builds are never node image candidates.
		{"gke-1351-gke1396004-cos-125-19216-104-126-c-pre-kmod", 0, false},
		{"gke-1351-gke1396004-cos-125-19216-104-126-c-pre-gvisor", 0, false},
		{"gke-1351-gke1396004-cos-125-19216-104-126-c-test", 0, false},
		{"gke-1353-gke1389001-cos-125-19216-220-106-c-cgpv1-pre", 0, false},
		{"ubuntu-gke-2404-1-35-amd64-v20260416", 0, false},
	}
	for _, tc := range cases {
		got, ok := classifyCOSImage(tc.name)
		require.Equal(t, tc.ok, ok, "name: %q", tc.name)
		require.Equal(t, tc.want, got, "name: %q", tc.name)
	}
}

func TestResolveImages_COS_LatestRanksVariantsAcrossPages(t *testing.T) {
	// Each variant is discovered on a different catalog page and published in stable order.
	catalog := &imageCatalog{pages: [][]*compute.Image{
		{{Name: "gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre", Status: "READY"}},
		{{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-nvda", Status: "READY"}},
		{{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", Status: "READY"}},
	}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Equal(t, []string{
		"projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre",
		"projects/gke-node-images/global/images/gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre",
		"projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-nvda",
	}, imageSources(got))
}

func TestResolveImages_COS_LatestRanksVariantsIndependently(t *testing.T) {
	// Catalog order is newest first, as images.list returns with creationTimestamp desc.
	catalog := &imageCatalog{pages: [][]*compute.Image{{
		// Newest build: its amd64 and NVIDIA images are READY, its arm64 sibling is still PENDING.
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", Status: "READY"},
		{Name: "gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre", Status: "PENDING"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-nvda", Status: "READY"},
		// Older build that only supplies the arm64 variant.
		{Name: "gke-1351-gke1390000-cos-125-19216-100-100-c-pre", Status: "READY"},
		{Name: "gke-1351-gke1390000-cos-arm64-125-19216-100-100-c-pre", Status: "READY"},
	}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Equal(t, []string{
		"projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre",
		"projects/gke-node-images/global/images/gke-1351-gke1390000-cos-arm64-125-19216-100-100-c-pre",
		"projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-nvda",
	}, imageSources(got))

	require.Equal(t, []string{OSArchAMD64Requirement}, got[0].Requirements.Get(v1.LabelArchStable).Values())
	require.Equal(t, v1.NodeSelectorOpDoesNotExist, got[0].Requirements.Get(v1alpha1.LabelInstanceGPUCount).Operator())
	require.Equal(t, []string{OSArchARM64Requirement}, got[1].Requirements.Get(v1.LabelArchStable).Values())
	require.Equal(t, []string{OSArchAMD64Requirement}, got[2].Requirements.Get(v1.LabelArchStable).Values())
	require.Equal(t, v1.NodeSelectorOpExists, got[2].Requirements.Get(v1alpha1.LabelInstanceGPUCount).Operator())
}

func TestResolveImages_COS_LatestFallsBackArbitrarilyFar(t *testing.T) {
	catalog := &imageCatalog{pages: [][]*compute.Image{{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", Status: "PENDING"},
		{Name: "gke-1351-gke1395000-cos-125-19216-103-100-c-pre", Status: "READY", Deprecated: &compute.DeprecationStatus{State: "DEPRECATED"}},
		{Name: "gke-1351-gke1394000-cos-125-19216-102-100-c-pre-kmod", Status: "READY"},
		{Name: "gke-1351-gke1393000-cos-125-19216-101-100-c-cgpv1-pre", Status: "READY"},
		{Name: "gke-1351-gke1392000-cos-125-19216-100-200-c-test", Status: "READY"},
		{Name: "gke-1351-gke1392500-cos-125-19216-100-250-c-pre", Status: "READY", Deprecated: &compute.DeprecationStatus{State: "DELETED"}},
		// A different Kubernetes patch is outside the cluster's scope.
		{Name: "gke-1353-gke1391000-cos-125-19216-100-100-c-pre", Status: "READY"},
		// Four builds older than the newest candidate, still eligible.
		{Name: "gke-1351-gke1390000-cos-125-19216-100-100-c-pre", Status: "READY"},
	}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Equal(t, []string{"projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre"}, imageSources(got))
}

func TestResolveImages_COS_LatestUsesBroadScopeWithoutClusterVersion(t *testing.T) {
	catalog := &imageCatalog{pages: [][]*compute.Image{{
		{Name: "gke-1353-gke1391000-cos-125-19216-100-100-c-pre", Status: "READY"},
		{Name: "gke-1351-gke1390000-cos-125-19216-100-100-c-pre", Status: "READY"},
	}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv)}
	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Equal(t, []string{"projects/gke-node-images/global/images/gke-1353-gke1391000-cos-125-19216-100-100-c-pre"}, imageSources(got))
}

func TestResolveImages_COS_LatestPublishesArm64Only(t *testing.T) {
	// GKE publishes each variant independently; arm64 arriving first must be usable on its own,
	// with the arm64 requirements that keep the scheduler off amd64 instance types.
	const arm64 = "gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre"
	catalog := &imageCatalog{pages: [][]*compute.Image{{{Name: arm64, Status: "READY"}}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Equal(t, []string{"projects/gke-node-images/global/images/" + arm64}, imageSources(got))
	require.Equal(t, []string{OSArchARM64Requirement}, got[0].Requirements.Get(v1.LabelArchStable).Values())
}

func TestResolveImages_COS_LatestOmitsUnpublishedVariants(t *testing.T) {
	catalog := &imageCatalog{pages: [][]*compute.Image{{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", Status: "READY"},
	}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Equal(t, []string{"projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre"}, imageSources(got))
}

func TestResolveImages_COS_LatestNoUsableCandidateIsUnavailable(t *testing.T) {
	catalog := &imageCatalog{pages: [][]*compute.Image{{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", Status: "PENDING"},
		{Name: "gke-1351-gke1390000-cos-125-19216-100-100-c-pre", Status: "READY", Deprecated: &compute.DeprecationStatus{State: "OBSOLETE"}},
	}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	_, err := p.ResolveImages(context.Background(), "latest")
	require.True(t, IsImageUnavailableError(err), "expected an unavailable error, got %v", err)
}

func TestResolveImages_COS_ValidatesListedCandidates(t *testing.T) {
	const (
		newest = "gke-1351-gke1396004-cos-125-19216-104-126-c-pre"
		older  = "gke-1351-gke1390000-cos-125-19216-100-100-c-pre"
	)
	for _, tc := range []struct {
		name    string
		image   *compute.Image
		status  int
		wantErr bool
	}{
		{"vanished", nil, http.StatusNotFound, false},
		{"pending", &compute.Image{Name: newest, Status: "PENDING"}, 0, false},
		{"deprecated", &compute.Image{Name: newest, Status: "READY", Deprecated: &compute.DeprecationStatus{State: "DEPRECATED"}}, 0, false},
		{"permission failure", nil, http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &imageCatalog{
				pages: [][]*compute.Image{{{Name: newest, Status: "READY"}, {Name: older, Status: "READY"}}},
				lookup: func(name string) (*compute.Image, int) {
					if name == newest {
						return tc.image, tc.status
					}
					return nil, 0
				},
			}
			srv := imageCatalogServer(t, catalog)
			t.Cleanup(srv.Close)
			p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
			got, err := p.ResolveImages(context.Background(), "latest")
			if tc.wantErr {
				require.Error(t, err)
				require.False(t, IsImageUnavailableError(err))
				require.False(t, IsImageResolutionError(err))
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"projects/gke-node-images/global/images/" + older}, imageSources(got))
		})
	}
}

func TestResolveMilestoneBaseCOSImage_SkipsPending(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", CreationTimestamp: "2025-04-02T00:00:00Z", Status: "PENDING"},
		{Name: "gke-1351-gke1390000-cos-125-19216-100-100-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z", Status: "READY"},
	}
	srv := cosImageServer(t, images)
	defer srv.Close()
	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	got, err := p.resolveMilestoneBaseCOSImage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre", got)
}

func TestResolveMilestoneBaseCOSImage_PicksNewestNonDeprecated(t *testing.T) {
	images := []*compute.Image{
		{
			Name:              "gke-1351-gke1396004-cos-125-19216-104-126-c-pre",
			CreationTimestamp: "2025-04-01T00:00:00Z",
			Status:            "READY",
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

	got, err := p.resolveMilestoneBaseCOSImage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre", got)
}

func TestResolveMilestoneBaseCOSImage_ExcludesSpecializedVariants(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre", CreationTimestamp: "2025-04-11T00:00:00Z"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-nvda", CreationTimestamp: "2025-04-10T00:00:00Z"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre-kmod", CreationTimestamp: "2025-04-09T00:00:00Z"},
		{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-test", CreationTimestamp: "2025-04-08T00:00:00Z"},
		// cgpv1 (cgroup v1) variant — must be excluded; GKE 1.29+ kubelet sets --fail-cgroupv1=true
		{Name: "gke-1353-gke1389001-cos-125-19216-220-106-c-cgpv1-pre", CreationTimestamp: "2025-04-07T00:00:00Z"},
		{Name: "gke-1351-gke1390000-cos-125-19216-100-100-c-pre", CreationTimestamp: "2025-03-01T00:00:00Z", Status: "READY"},
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

	got, err := p.resolveMilestoneBaseCOSImage(context.Background())
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
	require.Equal(t, `name=gke-*`, got)
}

func TestRenderVersion(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// K8s semver format (original)
		{"v1.35.1", "1-35-1"},
		{"1.35.1", "1-35-1"},
		// COS milestone.build.build.build format
		{"125.19216.104.126", "125-19216-104-126"},
		{"125.0.0.1", "125-0-0-1"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, renderVersion(tc.input), "input: %q", tc.input)
	}
}

func TestResolveImages_COS_PinnedVersion_InvalidFormat(t *testing.T) {
	p := &ContainerOptimizedOS{}
	for _, version := range []string{"125", "v1.35.1", "125.19216", "125.19216.104", "latest-1"} {
		_, err := p.ResolveImages(context.Background(), version)
		require.Errorf(t, err, "expected error for version %q", version)
		require.Contains(t, err.Error(), "invalid ContainerOptimizedOS version", "version %q", version)
	}
}

func TestResolveImages_COS_PinnedVersion_Valid(t *testing.T) {
	// Pin to the same version the fake latest image already carries — the regex
	// substitution runs but produces the same image name.
	images := []*compute.Image{
		{
			Name:              "gke-1351-gke1396004-cos-125-19216-104-126-c-pre",
			CreationTimestamp: "2025-04-01T00:00:00Z",
			Status:            "READY",
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

	got, err := p.ResolveImages(context.Background(), "125.19216.104.126")
	require.NoError(t, err)
	require.Len(t, got, 3) // x86, arm64, gpu

	srcs := make([]string, len(got))
	for i, img := range got {
		srcs[i] = img.SourceImage
	}
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre")
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre")
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-nvda")
}

func TestResolveImages_COS_PinnedVersion_DifferentMilestone(t *testing.T) {
	// Pin to a different milestone than what the latest image carries.
	// Verifies the regex substitution replaces the COS milestone+build segment.
	images := []*compute.Image{
		{
			Name:              "gke-1351-gke1396004-cos-125-19216-104-126-c-pre",
			CreationTimestamp: "2025-04-01T00:00:00Z",
			Status:            "READY",
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

	got, err := p.ResolveImages(context.Background(), "124.18935.44.10")
	require.NoError(t, err)
	require.Len(t, got, 3)

	srcs := make([]string, len(got))
	for i, img := range got {
		srcs[i] = img.SourceImage
	}
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-124-18935-44-10-c-pre")
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-arm64-124-18935-44-10-c-pre")
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-124-18935-44-10-c-nvda")
}

func TestParseGKEVersion(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantKey   string
		wantBuild string
		wantOK    bool
	}{
		{"standard", "1.34.6-gke.1068000", "1346", "1068000", true},
		{"with v prefix", "v1.35.1-gke.1396004", "1351", "1396004", true},
		{"latest is not GKE", "latest", "", "", false},
		{"milestone pin is not GKE", "1351.19216.104.126", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, build, ok := ParseGKEVersion(tc.input)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantKey, key)
			require.Equal(t, tc.wantBuild, build)
		})
	}
}

func TestResolveExactBuildCOSImage_PicksMatchingBuild(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z", Status: "READY"},
		// different build — must be excluded by filter (test server returns all, usability check won't exclude)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: images}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv)}
	got, err := p.resolveExactBuildCOSImage(context.Background(), "1346", "1068000")
	require.NoError(t, err)
	require.Equal(t, "projects/gke-node-images/global/images/gke-1346-gke1068000-cos-125-19216-220-72-c-pre", got)
}

func TestResolveExactBuildCOSImage_SkipsPending(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", Status: "PENDING", CreationTimestamp: "2025-04-02T00:00:00Z"},
		{Name: "gke-1346-gke1068000-cos-125-19216-220-71-c-pre", Status: "READY", CreationTimestamp: "2025-04-01T00:00:00Z"},
	}
	srv := cosImageServer(t, images)
	defer srv.Close()
	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv)}
	got, err := p.resolveExactBuildCOSImage(context.Background(), "1346", "1068000")
	require.NoError(t, err)
	require.Equal(t, "projects/gke-node-images/global/images/gke-1346-gke1068000-cos-125-19216-220-71-c-pre", got)
}

func TestResolveImages_GKEVersion_FindsMatchingBuildAcrossPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			_ = json.NewEncoder(w).Encode(&compute.ImageList{NextPageToken: "next", Items: []*compute.Image{
				{Name: "gke-1346-gke9999999-cos-125-19216-220-72-c-pre", CreationTimestamp: "2026-04-20T00:00:00Z", Status: "READY"},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(&compute.ImageList{Items: []*compute.Image{
			{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", CreationTimestamp: "2026-04-19T00:00:00Z", Status: "READY"},
		}})
	}))
	defer srv.Close()
	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv)}
	got, err := p.ResolveImages(context.Background(), "1.34.6-gke.1068000")
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.Equal(t, "projects/gke-node-images/global/images/gke-1346-gke1068000-cos-125-19216-220-72-c-pre", got[0].SourceImage)
}

func TestResolveExactBuildCOSImage_ReturnsErrorOnMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: []*compute.Image{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv)}
	_, err := p.resolveExactBuildCOSImage(context.Background(), "1346", "9999999")
	require.Error(t, err)
	require.Contains(t, err.Error(), "gke9999999")
	require.True(t, IsImageUnavailableError(err), "a build without a published COS image is a publication lag, not a configuration error: %v", err)
}

func TestResolveImages_GKEVersion_UsesExactBuild(t *testing.T) {
	images := []*compute.Image{
		{Name: "gke-1346-gke1068000-cos-125-19216-220-72-c-pre", CreationTimestamp: "2025-04-01T00:00:00Z", Status: "READY"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: images}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &ContainerOptimizedOS{computeService: buildComputeService(t, srv)}
	imgs, err := p.ResolveImages(context.Background(), "1.34.6-gke.1068000")
	require.NoError(t, err)
	require.Len(t, imgs, 3) // amd64, arm64, gpu

	var srcs []string
	for _, img := range imgs {
		srcs = append(srcs, img.SourceImage)
	}
	require.Contains(t, srcs, "projects/gke-node-images/global/images/gke-1346-gke1068000-cos-125-19216-220-72-c-pre")
}
