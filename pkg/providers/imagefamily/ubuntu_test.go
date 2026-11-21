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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestResolveImages_UbuntuRequiresMasterSnapshot(t *testing.T) {
	p := &Ubuntu{release: "2404"}
	_, err := p.ResolveImages(context.Background(), "latest")
	require.ErrorContains(t, err, "API-server version is required")
}

func TestResolveImages_Ubuntu_FindsBothArchitecturesAcrossPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("pageToken") {
		case "":
			_ = json.NewEncoder(w).Encode(&compute.ImageList{NextPageToken: "second", Items: []*compute.Image{
				{Name: "ubuntu-gke-2404-1-34-amd64-v20260420", CreationTimestamp: "2026-04-20T00:00:00Z"},
			}})
		case "second":
			_ = json.NewEncoder(w).Encode(&compute.ImageList{NextPageToken: "third", Items: []*compute.Image{
				{Name: "ubuntu-gke-2404-1-35-amd64-v20260416", CreationTimestamp: "2026-04-16T00:00:00Z", Status: "READY"},
			}})
		case "third":
			_ = json.NewEncoder(w).Encode(&compute.ImageList{NextPageToken: "fourth", Items: []*compute.Image{
				{Name: "ubuntu-gke-2404-1-35-arm64-v20260416", CreationTimestamp: "2026-04-15T00:00:00Z", Status: "READY"},
			}})
		default:
			_ = json.NewEncoder(w).Encode(&compute.ImageList{})
		}
	}))
	defer srv.Close()
	p := &Ubuntu{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.35.1"}, release: "2404"}
	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Equal(t, []string{
		"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20260416",
		"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-arm64-v20260416",
	}, imageSources(got))
}

func TestResolveImages_Ubuntu_PinnedVersion_Valid(t *testing.T) {
	images := []*compute.Image{
		// newer builds that must be ignored in favor of the pinned date
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420", CreationTimestamp: "2026-04-20T00:00:00Z"},
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260420", CreationTimestamp: "2026-04-20T00:05:00Z"},
		// the pinned date, present for both architectures
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260416", CreationTimestamp: "2026-04-16T00:00:00Z"},
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260416", CreationTimestamp: "2026-04-16T00:05:00Z"},
	}
	srv := imageListServer(t, images)
	defer srv.Close()

	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.35.1-gke.123"},
		computeService: buildComputeService(t, srv),
		release:        "2404",
	}

	// Each architecture resolves to that exact catalog image, not the newest.
	got, err := p.ResolveImages(context.Background(), "v20260416")
	require.NoError(t, err)
	require.Len(t, got, 2) // amd64, arm64

	srcs := imageSources(got)
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20260416")
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-arm64-v20260416")
}

// A pinned date present for one architecture but absent for the other must fail resolution,
// not return a single-arch set that reads as ImagesReady=True — the arm64 image is selected
// from the catalog, never constructed by rewriting the amd64 date.
func TestResolveImages_Ubuntu_PinnedVersion_MissingArm64_IsResolutionError(t *testing.T) {
	images := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260416", CreationTimestamp: "2026-04-16T00:00:00Z"},
		// arm64 exists only at another date, so the pinned arm64 image is absent
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260420", CreationTimestamp: "2026-04-20T00:00:00Z"},
	}
	srv := imageListServer(t, images)
	defer srv.Close()

	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.35.1-gke.123"},
		computeService: buildComputeService(t, srv),
		release:        "2404",
	}

	_, err := p.ResolveImages(context.Background(), "v20260416")
	require.Error(t, err)
	require.True(t, IsImageResolutionError(err), "expected an imageResolutionError, got %v", err)
}

func TestResolveImages_Ubuntu_PinnedVersion_PendingArm64(t *testing.T) {
	images := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260416", Status: "READY"},
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260416", Status: "PENDING"},
	}
	srv := imageListServer(t, images)
	defer srv.Close()
	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.35.1-gke.123"}, computeService: buildComputeService(t, srv), release: "2404"}
	_, err := p.ResolveImages(context.Background(), "v20260416")
	require.Error(t, err)
	// Ubuntu discovery only admits READY images, so a catalog-PENDING sibling leaves the required
	// arm64 architecture unresolved: that is a configuration-shaped resolution failure, unlike a
	// listed READY image that turns out to be PENDING on lookup (TestAliasTerm_UbuntuUnreadySiblingStillFailsResolution).
	require.True(t, IsImageResolutionError(err), "expected an imageResolutionError, got %v", err)
	require.False(t, IsImageUnavailableError(err))
}

// Ubuntu completeness is unchanged by the COS partial-set policy: an arm64 image that stops
// being READY after it was listed must fail resolution, not publish an amd64-only set.
func TestAliasTerm_UbuntuUnreadySiblingStillFailsResolution(t *testing.T) {
	const (
		amd64 = "ubuntu-gke-2404-1-35-amd64-v20260416"
		arm64 = "ubuntu-gke-2404-1-35-arm64-v20260416"
	)
	catalog := &imageCatalog{
		pages: [][]*compute.Image{{
			{Name: amd64, Status: "READY"},
			{Name: arm64, Status: "READY"},
		}},
		lookup: func(name string) (*compute.Image, int) {
			if name == arm64 {
				return &compute.Image{Name: arm64, Status: "PENDING"}, 0
			}
			return nil, 0
		},
	}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()

	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.1"}, &stubGKEProvider{})
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "Ubuntu2404@latest"}},
	}}

	_, err := p.List(context.Background(), nc)
	require.ErrorContains(t, err, "PENDING")
	require.False(t, IsImageResolutionError(err))
	require.True(t, IsImageUnavailableError(err), "a pending Ubuntu sibling is a publication lag, not a configuration or API failure: %v", err)
}

func TestResolveImages_Ubuntu_PinnedVersion_InvalidFormat(t *testing.T) {
	p := &Ubuntu{}
	for _, version := range []string{
		"20260416", "v202604161", "v2026041", "vABCDEFGH", "v20260416-extra",
		"v20251218ab", "v20251218A", "v20251218-a", "v20251218_a", "va20251218", "20251218a", "v2025121a",
	} {
		_, err := p.ResolveImages(context.Background(), version)
		require.Errorf(t, err, "expected error for version %q", version)
		require.Contains(t, err.Error(), "invalid Ubuntu version", "version %q", version)
	}
}

// TestUbuntuIsUsableForArch protects the clean-name allowlist per release and architecture: a
// READY, non-deprecated image with the release own arch token (2204 amd64 has none) is usable
// (including the -v<date>a letter suffix), and no other architecture, release or variant build is,
// whether the variant token is a trailing suffix (the real GCP form) or an infix.
func TestUbuntuIsUsableForArch(t *testing.T) {
	tests := []struct {
		release, arch string
		usable        []string
		unusable      []string
	}{
		{
			release: "2404", arch: OSArchAMD64Requirement,
			usable: []string{"ubuntu-gke-2404-1-35-amd64-v20260420", "ubuntu-gke-2404-1-36-amd64-v20251218a"},
			unusable: []string{
				"ubuntu-gke-2404-1-35-arm64-v20260420",
				"ubuntu-gke-2204-1-34-v20260420",
				"ubuntu-gke-2604-1-38-amd64-v20261119",
				"ubuntu-gke-2404-1-35-amd64-v20260722-tpu",
				"ubuntu-gke-2404-1-35-amd64-v20260722-cgroupsv1",
				"ubuntu-gke-2404-1-35-amd64-v20260722-linux64k",
				"ubuntu-gke-2404-1-35-amd64-v20260722-test",
				"ubuntu-gke-2404-1-35-amd64-v20260722-devel",
				"ubuntu-gke-2404-1-35-amd64-v20260722-tpu-test",
				"ubuntu-gke-2404-1-35-amd64-v20260722-custom-test",
				"ubuntu-gke-2404-1-35-amd64-cgroupsv1-v20260420",
				"ubuntu-gke-2404-1-35-amd64-tpu-v20260420",
				// per-GKE-patch images in the Ubuntu image project are never "latest" candidates
				"gke-1365-gke2391000-ubuntu-2404-1-36-amd64-v20260528-c-pre",
			},
		},
		{
			release: "2404", arch: OSArchARM64Requirement,
			usable: []string{"ubuntu-gke-2404-1-35-arm64-v20260420", "ubuntu-gke-2404-1-35-arm64-v20251218a"},
			unusable: []string{
				"ubuntu-gke-2404-1-35-amd64-v20260420",
				"ubuntu-gke-2404-1-35-arm64-v20260722-linux64k",
				"ubuntu-gke-2404-1-37-arm64-v20261112-64k",
			},
		},
		{
			// 2204 amd64 names carry no arch token.
			release: "2204", arch: OSArchAMD64Requirement,
			usable: []string{"ubuntu-gke-2204-1-31-v20260420", "ubuntu-gke-2204-1-31-v20251218a"},
			unusable: []string{
				"ubuntu-gke-2204-1-31-arm64-v20260420",
				"ubuntu-gke-2204-1-31-v20260722-tpu",
				"ubuntu-gke-2204-1-31-v20260722-cgroupsv1",
				"ubuntu-gke-2204-1-31-v20260722-cgroupsv1-devel",
				"ubuntu-gke-2204-1-31-v20260722-devel",
				"ubuntu-gke-2404-1-31-amd64-v20260420",
			},
		},
		{
			release: "2204", arch: OSArchARM64Requirement,
			usable:   []string{"ubuntu-gke-2204-1-31-arm64-v20260420"},
			unusable: []string{"ubuntu-gke-2204-1-31-v20260420", "ubuntu-gke-2204-1-31-arm64-v20260722-linux64k"},
		},
		{
			release: "2604", arch: OSArchAMD64Requirement,
			usable: []string{"ubuntu-gke-2604-1-38-amd64-v20261119", "ubuntu-gke-2604-1-37-amd64-v20260925a"},
			unusable: []string{
				"ubuntu-gke-2604-1-38-arm64-v20261119",
				"ubuntu-gke-2404-1-37-amd64-v20261119",
				"ubuntu-gke-2604-1-38-amd64-v20261119-hwe",
				"ubuntu-gke-2604-1-38-amd64-v20261119-tpu",
			},
		},
		{
			release: "2604", arch: OSArchARM64Requirement,
			usable: []string{"ubuntu-gke-2604-1-38-arm64-v20261119"},
			unusable: []string{
				"ubuntu-gke-2604-1-38-amd64-v20261119",
				"ubuntu-gke-2604-1-38-arm64-v20261119-hwe",
				"ubuntu-gke-2604-1-38-arm64-v20261119-64k",
				"ubuntu-gke-2604-1-38-arm64-v20261113-acs",
				"ubuntu-gke-2604-1-37-arm64-v20260925-linux64k",
			},
		},
	}
	for _, tt := range tests {
		u := &Ubuntu{release: tt.release}
		for _, name := range tt.usable {
			require.True(t, u.isUsableForArch(&compute.Image{Name: name, Status: "READY"}, tt.arch),
				"%s %s: expected %q to be usable", tt.release, tt.arch, name)
		}
		for _, name := range tt.unusable {
			require.False(t, u.isUsableForArch(&compute.Image{Name: name, Status: "READY"}, tt.arch),
				"%s %s: expected %q to be unusable", tt.release, tt.arch, name)
		}
	}

	// Retired or unfinished images are never usable, even with a clean name.
	u := &Ubuntu{release: "2404"}
	const clean = "ubuntu-gke-2404-1-35-amd64-v20260420"
	require.True(t, u.isUsableForArch(&compute.Image{Name: clean, Status: "READY", Deprecated: &compute.DeprecationStatus{State: ""}}, OSArchAMD64Requirement))
	require.False(t, u.isUsableForArch(&compute.Image{Name: clean, Status: "PENDING"}, OSArchAMD64Requirement))
	for _, state := range []string{"DEPRECATED", "OBSOLETE", "DELETED"} {
		require.False(t, u.isUsableForArch(&compute.Image{Name: clean, Status: "READY", Deprecated: &compute.DeprecationStatus{State: state}}, OSArchAMD64Requirement), state)
	}
}

// Regression coverage for arm64 / trailing-variant-suffix resolution: each architecture is
// resolved independently to a real image, and a missing architecture fails resolution.

func imageListServer(t *testing.T, images []*compute.Image) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: readyTestImages(images)}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func imageSources(imgs Images) []string {
	srcs := make([]string, len(imgs))
	for i, img := range imgs {
		srcs[i] = img.SourceImage
	}
	return srcs
}

func TestResolveImages_Ubuntu_SelectsLetterSuffixVersion(t *testing.T) {
	images := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20251218a", CreationTimestamp: "2025-12-18T02:00:00Z"},
		{Name: "ubuntu-gke-2404-1-35-arm64-v20251218a", CreationTimestamp: "2025-12-18T02:05:00Z"},
	}
	srv := imageListServer(t, images)
	defer srv.Close()

	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.35.1-gke.123"},
		computeService: buildComputeService(t, srv),
		release:        "2404",
	}

	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	srcs := imageSources(got)
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20251218a")
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-arm64-v20251218a")
}

// Core bug: when the newest amd64 image is a trailing -tpu variant, amd64 must resolve
// to the newest real (non-variant) amd64 image, and arm64 must resolve to the real
// -arm64- image — never a name derived from the amd64 pick.
func TestResolveImages_Ubuntu_ResolvesRealArm64_WhenNewestAmd64IsTpuSuffix(t *testing.T) {
	images := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722", CreationTimestamp: "2026-07-22T14:04:32Z"},
		// newest amd64-named build, but a -tpu variant → must not be selected
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-tpu", CreationTimestamp: "2026-07-22T14:04:40Z"},
		// real arm64 image, newest overall
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260722", CreationTimestamp: "2026-07-22T14:05:00Z"},
	}
	srv := imageListServer(t, images)
	defer srv.Close()

	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.35.1-gke.123"},
		computeService: buildComputeService(t, srv),
		release:        "2404",
	}

	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Len(t, got, 2)

	srcs := imageSources(got)
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20260722")
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-arm64-v20260722")
	for _, s := range srcs {
		require.NotContains(t, s, "-tpu", "no variant image should be selected, got %q", s)
	}
}

// Honest readiness: if no usable arm64 image exists for the family, resolution fails
// rather than returning an amd64-only set (which would read as ImagesReady=True).
func TestResolveImages_Ubuntu_MissingArm64_IsResolutionError(t *testing.T) {
	images := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722", CreationTimestamp: "2026-07-22T14:04:32Z"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-tpu", CreationTimestamp: "2026-07-22T14:04:40Z"},
		// no arm64 image present
	}
	srv := imageListServer(t, images)
	defer srv.Close()

	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.35.1-gke.123"},
		computeService: buildComputeService(t, srv),
		release:        "2404",
	}

	_, err := p.ResolveImages(context.Background(), "latest")
	require.Error(t, err)
	require.True(t, IsImageResolutionError(err), "expected an imageResolutionError, got %v", err)
}

// 2204 equivalent: amd64 has no -amd64- token; arm64 is a distinct -arm64- image.
func TestResolveImages_Ubuntu2204_ResolvesRealArm64_WhenNewestAmd64IsTpuSuffix(t *testing.T) {
	images := []*compute.Image{
		{Name: "ubuntu-gke-2204-1-34-v20260722", CreationTimestamp: "2026-07-22T14:04:32Z"},
		{Name: "ubuntu-gke-2204-1-34-v20260722-tpu", CreationTimestamp: "2026-07-22T14:04:40Z"},
		{Name: "ubuntu-gke-2204-1-34-arm64-v20260722", CreationTimestamp: "2026-07-22T14:05:00Z"},
	}
	srv := imageListServer(t, images)
	defer srv.Close()

	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.34.1-gke.123"},
		computeService: buildComputeService(t, srv),
		release:        "2204",
	}

	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Len(t, got, 2)

	srcs := imageSources(got)
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2204-1-34-v20260722")
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2204-1-34-arm64-v20260722")
	for _, s := range srcs {
		require.NotContains(t, s, "-tpu", "no variant image should be selected, got %q", s)
	}
}

// TestUbuntuImageScope protects which image project and name prefix a cluster minor selects:
// GKE publishes Ubuntu images for 1.36+ in gke-ubuntu-node-images and for earlier minors in
// ubuntu-os-gke-cloud.
func TestUbuntuImageScope(t *testing.T) {
	tests := []struct {
		release, version, project, prefix string
	}{
		{"2404", "v1.31.14-gke.1", "ubuntu-os-gke-cloud", "ubuntu-gke-2404-1-31"},
		{"2404", "v1.35.9-gke.1673000", "ubuntu-os-gke-cloud", "ubuntu-gke-2404-1-35"},
		{"2404", "1.36.0", "gke-ubuntu-node-images", "ubuntu-gke-2404-1-36"},
		{"2404", "v1.36.5-gke.2262000", "gke-ubuntu-node-images", "ubuntu-gke-2404-1-36"},
		{"2404", "v1.37.2-gke.1042000", "gke-ubuntu-node-images", "ubuntu-gke-2404-1-37"},
		{"2604", "v1.38.0-gke.4814000+preview", "gke-ubuntu-node-images", "ubuntu-gke-2604-1-38"},
		{"2204", "v1.34.7-gke.123", "ubuntu-os-gke-cloud", "ubuntu-gke-2204-1-34"},
	}
	for _, tt := range tests {
		project, prefix, err := (&Ubuntu{release: tt.release}).imageScope(tt.version)
		require.NoError(t, err, tt.version)
		require.Equal(t, tt.project, project, "%s %s", tt.release, tt.version)
		require.Equal(t, tt.prefix, prefix, "%s %s", tt.release, tt.version)
	}

	_, _, err := (&Ubuntu{release: "2404"}).imageScope("not-a-version")
	require.Error(t, err, "an unparseable cluster version must not guess a project")
}

// projectImageServer serves one catalog per image project and records the projects listed.
func projectImageServer(t *testing.T, catalogs map[string][]*compute.Image) (*httptest.Server, *[]string) {
	t.Helper()
	var listed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		project := ""
		for i, part := range parts {
			if part == "projects" && i+1 < len(parts) {
				project = parts[i+1]
			}
		}
		listed = append(listed, project)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&compute.ImageList{Items: readyTestImages(catalogs[project])})
	}))
	t.Cleanup(srv.Close)
	return srv, &listed
}

// TestResolveImages_Ubuntu_ListsProjectForClusterMinor protects that images are listed in, and
// resolved from, only the project GKE uses for the cluster's minor, even when the other project
// holds a newer image of the same release.
func TestResolveImages_Ubuntu_ListsProjectForClusterMinor(t *testing.T) {
	legacy := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20261113", CreationTimestamp: "2026-11-13T00:00:00Z"},
		{Name: "ubuntu-gke-2404-1-35-arm64-v20261113", CreationTimestamp: "2026-11-13T00:05:00Z"},
		{Name: "ubuntu-gke-2404-1-36-amd64-v20261120", CreationTimestamp: "2026-11-20T00:00:00Z"},
		{Name: "ubuntu-gke-2404-1-36-arm64-v20261120", CreationTimestamp: "2026-11-20T00:05:00Z"},
	}
	// Served newest first, as images.list orders them; the older clean builds must lose.
	current := []*compute.Image{
		{Name: "gke-1365-gke2391000-ubuntu-2404-1-36-amd64-v20260528-c-pre", CreationTimestamp: "2026-11-19T00:00:00Z"},
		{Name: "gke-1365-gke2391000-ubuntu-2404-1-36-arm64-v20260528-c-pre", CreationTimestamp: "2026-11-19T00:05:00Z"},
		{Name: "ubuntu-gke-2404-1-36-arm64-v20261117", CreationTimestamp: "2026-11-17T00:05:00Z"},
		{Name: "ubuntu-gke-2404-1-36-amd64-v20261117", CreationTimestamp: "2026-11-17T00:00:00Z"},
		{Name: "ubuntu-gke-2404-1-36-amd64-v20261117-tpu", CreationTimestamp: "2026-11-17T00:03:00Z", Status: "READY"},
		{Name: "ubuntu-gke-2404-1-36-arm64-v20261112", CreationTimestamp: "2026-11-12T00:05:00Z"},
		{Name: "ubuntu-gke-2404-1-36-amd64-v20261112", CreationTimestamp: "2026-11-12T00:00:00Z"},
	}
	tests := []struct {
		name, version string
		want          []string
	}{
		{"before 1.36 uses the legacy project", "v1.35.9-gke.1673000", []string{
			"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20261113",
			"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-arm64-v20261113",
		}},
		{"1.36 uses the current project", "v1.36.5-gke.2262000", []string{
			"projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2404-1-36-amd64-v20261117",
			"projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2404-1-36-arm64-v20261117",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, listed := projectImageServer(t, map[string][]*compute.Image{
				"ubuntu-os-gke-cloud": legacy, "gke-ubuntu-node-images": current,
			})
			p := &Ubuntu{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: tt.version}, release: "2404"}
			got, err := p.ResolveImages(context.Background(), "latest")
			require.NoError(t, err)
			require.Equal(t, tt.want, imageSources(got))
			require.Equal(t, []string{strings.Split(strings.TrimPrefix(tt.want[0], "projects/"), "/")[0]}, *listed,
				"only the selected project may be listed")
		})
	}
}

// TestResolveImages_Ubuntu2604 protects the 26.04 release: clean amd64 and arm64 images resolve
// independently of the -hwe/-tpu/-64k/-acs variants GKE publishes beside them.
func TestResolveImages_Ubuntu2604(t *testing.T) {
	srv, _ := projectImageServer(t, map[string][]*compute.Image{"gke-ubuntu-node-images": {
		{Name: "ubuntu-gke-2604-1-38-amd64-v20261119-tpu", CreationTimestamp: "2026-11-19T00:03:00Z"},
		{Name: "ubuntu-gke-2604-1-38-amd64-v20261119-hwe", CreationTimestamp: "2026-11-19T00:02:00Z"},
		{Name: "ubuntu-gke-2604-1-38-amd64-v20261119", CreationTimestamp: "2026-11-19T00:01:00Z"},
		{Name: "ubuntu-gke-2604-1-38-arm64-v20261119-64k", CreationTimestamp: "2026-11-19T00:06:00Z"},
		{Name: "ubuntu-gke-2604-1-38-arm64-v20261119-hwe", CreationTimestamp: "2026-11-19T00:05:00Z"},
		{Name: "ubuntu-gke-2604-1-38-arm64-v20261113-acs", CreationTimestamp: "2026-11-13T00:05:00Z"},
		{Name: "ubuntu-gke-2604-1-38-arm64-v20261119", CreationTimestamp: "2026-11-19T00:04:00Z"},
	}})
	p := &Ubuntu{computeService: buildComputeService(t, srv), versionProvider: &fakeVersionProvider{version: "v1.38.0-gke.4814000"}, release: "2604"}
	got, err := p.ResolveImages(context.Background(), "latest")
	require.NoError(t, err)
	require.Equal(t, []string{
		"projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2604-1-38-amd64-v20261119",
		"projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2604-1-38-arm64-v20261119",
	}, imageSources(got))
	for _, image := range got {
		require.Equal(t, "1.38.0-gke.4814000", image.KubernetesVersion)
	}
}

// TestResolveImages_Ubuntu_NoImagesForReleaseOnClusterMinor protects the failure when GKE does not
// publish a release for the cluster's minor (24.04 on 1.38, 22.04 on 1.35): resolution fails with
// the project and release named instead of falling back to another release or project.
func TestResolveImages_Ubuntu_NoImagesForReleaseOnClusterMinor(t *testing.T) {
	srv, _ := projectImageServer(t, map[string][]*compute.Image{
		"gke-ubuntu-node-images": {
			{Name: "ubuntu-gke-2604-1-38-amd64-v20261119", CreationTimestamp: "2026-11-19T00:01:00Z"},
			{Name: "ubuntu-gke-2604-1-38-arm64-v20261119", CreationTimestamp: "2026-11-19T00:04:00Z"},
		},
		"ubuntu-os-gke-cloud": {
			{Name: "ubuntu-gke-2404-1-35-amd64-v20261113", CreationTimestamp: "2026-11-13T00:00:00Z"},
			{Name: "ubuntu-gke-2404-1-35-arm64-v20261113", CreationTimestamp: "2026-11-13T00:05:00Z"},
		},
	})
	svc := buildComputeService(t, srv)

	_, err := (&Ubuntu{computeService: svc, versionProvider: &fakeVersionProvider{version: "v1.38.0-gke.1"}, release: "2404"}).ResolveImages(context.Background(), "latest")
	require.True(t, IsImageResolutionError(err))
	require.ErrorContains(t, err, "ubuntu-gke-2404")
	require.ErrorContains(t, err, "gke-ubuntu-node-images")

	_, err = (&Ubuntu{computeService: svc, versionProvider: &fakeVersionProvider{version: "v1.35.9-gke.1"}, release: "2204"}).ResolveImages(context.Background(), "latest")
	require.True(t, IsImageResolutionError(err))
	require.ErrorContains(t, err, "only through Kubernetes 1.31", "the catalog limit must be explained")
}

// TestSameImage protects drift across the Ubuntu image projects: GKE serves identical image names
// from both, so a node launched from one is not drifted by the other, but a different image,
// a non-Ubuntu project, or a COS image still is.
func TestSameImage(t *testing.T) {
	const (
		legacy  = "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-36-amd64-v20261117"
		current = "projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2404-1-36-amd64-v20261117"
	)
	require.True(t, SameImage(legacy, legacy))
	require.True(t, SameImage("https://www.googleapis.com/compute/v1/"+legacy, current), "legacy node, current status")
	require.True(t, SameImage("https://www.googleapis.com/compute/v1/"+current, legacy), "current node, legacy status")

	require.False(t, SameImage("https://www.googleapis.com/compute/v1/"+legacy,
		"projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2404-1-36-amd64-v20261118"), "different build")
	require.False(t, SameImage("https://www.googleapis.com/compute/v1/"+legacy,
		"projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2404-1-36-arm64-v20261117"), "different arch")
	require.False(t, SameImage("https://www.googleapis.com/compute/v1/projects/other/global/images/ubuntu-gke-2404-1-36-amd64-v20261117", current),
		"same name in an unrelated project")
	cos := "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre"
	require.True(t, SameImage("https://www.googleapis.com/compute/v1/"+cos, cos))
	require.False(t, SameImage("https://www.googleapis.com/compute/v1/projects/other/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre", cos))
}

func TestResolveImages_Ubuntu_PinnedVersion_LetterSuffix(t *testing.T) {
	tests := []struct {
		name    string
		release string
		images  []*compute.Image
		version string
		want    []string
	}{
		{
			name:    "2404 letter pin resolves the exact lettered build",
			release: "2404",
			images: []*compute.Image{
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218", CreationTimestamp: "2025-12-18T00:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218", CreationTimestamp: "2025-12-18T00:05:00Z"},
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218a", CreationTimestamp: "2025-12-18T02:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218a", CreationTimestamp: "2025-12-18T02:05:00Z"},
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218b", CreationTimestamp: "2025-12-18T04:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218b", CreationTimestamp: "2025-12-18T04:05:00Z"},
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218ab", CreationTimestamp: "2025-12-18T06:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218ab", CreationTimestamp: "2025-12-18T06:05:00Z"},
			},
			version: "v20251218a",
			want: []string{
				"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-32-amd64-v20251218a",
				"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-32-arm64-v20251218a",
			},
		},
		{
			name:    "2204 letter pin resolves the exact lettered build",
			release: "2204",
			images: []*compute.Image{
				{Name: "ubuntu-gke-2204-1-32-v20251218", CreationTimestamp: "2025-12-18T00:00:00Z"},
				{Name: "ubuntu-gke-2204-1-32-arm64-v20251218", CreationTimestamp: "2025-12-18T00:05:00Z"},
				{Name: "ubuntu-gke-2204-1-32-v20251218a", CreationTimestamp: "2025-12-18T02:00:00Z"},
				{Name: "ubuntu-gke-2204-1-32-arm64-v20251218a", CreationTimestamp: "2025-12-18T02:05:00Z"},
				{Name: "ubuntu-gke-2204-1-32-v20251218b", CreationTimestamp: "2025-12-18T04:00:00Z"},
				{Name: "ubuntu-gke-2204-1-32-arm64-v20251218b", CreationTimestamp: "2025-12-18T04:05:00Z"},
				{Name: "ubuntu-gke-2204-1-32-v20251218ab", CreationTimestamp: "2025-12-18T06:00:00Z"},
				{Name: "ubuntu-gke-2204-1-32-arm64-v20251218ab", CreationTimestamp: "2025-12-18T06:05:00Z"},
			},
			version: "v20251218a",
			want: []string{
				"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2204-1-32-v20251218a",
				"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2204-1-32-arm64-v20251218a",
			},
		},
		{
			name:    "2404 letterless pin ignores a newer lettered sibling",
			release: "2404",
			images: []*compute.Image{
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218", CreationTimestamp: "2025-12-18T00:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218", CreationTimestamp: "2025-12-18T00:05:00Z"},
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218a", CreationTimestamp: "2025-12-18T02:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218a", CreationTimestamp: "2025-12-18T02:05:00Z"},
			},
			version: "v20251218",
			want: []string{
				"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-32-amd64-v20251218",
				"projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-32-arm64-v20251218",
			},
		},
		{
			name:    "2404 letterless pin with only a lettered build fails",
			release: "2404",
			images: []*compute.Image{
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218a", CreationTimestamp: "2025-12-18T02:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218a", CreationTimestamp: "2025-12-18T02:05:00Z"},
			},
			version: "v20251218",
		},
		{
			name:    "2204 letterless pin with only a lettered build fails",
			release: "2204",
			images: []*compute.Image{
				{Name: "ubuntu-gke-2204-1-32-v20251218a", CreationTimestamp: "2025-12-18T02:00:00Z"},
				{Name: "ubuntu-gke-2204-1-32-arm64-v20251218a", CreationTimestamp: "2025-12-18T02:05:00Z"},
			},
			version: "v20251218",
		},
		{
			name:    "2404 letter pin does not match a different letter",
			release: "2404",
			images: []*compute.Image{
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218b", CreationTimestamp: "2025-12-18T04:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218b", CreationTimestamp: "2025-12-18T04:05:00Z"},
			},
			version: "v20251218a",
		},
		{
			name:    "2404 letter pin fails when arm64 has only the letterless build",
			release: "2404",
			images: []*compute.Image{
				{Name: "ubuntu-gke-2404-1-32-amd64-v20251218a", CreationTimestamp: "2025-12-18T02:00:00Z"},
				{Name: "ubuntu-gke-2404-1-32-arm64-v20251218", CreationTimestamp: "2025-12-18T00:05:00Z"},
			},
			version: "v20251218a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := imageListServer(t, tt.images)
			defer srv.Close()
			p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.32.1-gke.123"}, computeService: buildComputeService(t, srv), release: tt.release}

			got, err := p.ResolveImages(context.Background(), tt.version)
			if tt.want == nil {
				require.Error(t, err)
				require.True(t, IsImageResolutionError(err), "expected an imageResolutionError, got %v", err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, imageSources(got))
		})
	}
}

// TestListUbuntuOSVersionSelectsRelease protects how an Ubuntu term chooses its release: the
// osVersion of family Ubuntu picks it, the deprecated per-release families and the legacy alias
// resolve exactly as before, and an unknown release is a configuration error naming the supported
// ones rather than a fallback to another release.
func TestListUbuntuOSVersionSelectsRelease(t *testing.T) {
	catalog := &imageCatalog{pages: [][]*compute.Image{{
		{Name: "ubuntu-gke-2604-1-37-arm64-v20260925", Status: "READY"},
		{Name: "ubuntu-gke-2604-1-37-amd64-v20260925", Status: "READY"},
		{Name: "ubuntu-gke-2404-1-37-arm64-v20261112", Status: "READY"},
		{Name: "ubuntu-gke-2404-1-37-amd64-v20261112", Status: "READY"},
	}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()
	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.37.2-gke.1042000"}, &stubGKEProvider{})

	list := func(term v1alpha1.ImageSelectorTerm) (Images, error) {
		return p.List(context.Background(), &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{term},
		}})
	}
	const (
		amd2404 = "projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2404-1-37-amd64-v20261112"
		arm2404 = "projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2404-1-37-arm64-v20261112"
	)

	got, err := list(v1alpha1.ImageSelectorTerm{Family: "Ubuntu", OSVersion: "26.04", Version: "latest"})
	require.NoError(t, err)
	require.Equal(t, []string{
		"projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2604-1-37-amd64-v20260925",
		"projects/gke-ubuntu-node-images/global/images/ubuntu-gke-2604-1-37-arm64-v20260925",
	}, imageSources(got))

	for name, term := range map[string]v1alpha1.ImageSelectorTerm{
		"osVersion 24.04":      {Family: "Ubuntu", OSVersion: "24.04", Version: "latest"},
		"deprecated family":    {Family: "Ubuntu2404", Version: "latest"},
		"legacy Ubuntu alias":  {Alias: "Ubuntu@latest"},     //nolint:staticcheck
		"legacy release alias": {Alias: "Ubuntu2404@latest"}, //nolint:staticcheck
	} {
		got, err := list(term)
		require.NoError(t, err, name)
		require.Equal(t, []string{amd2404, arm2404}, imageSources(got), name)
	}

	_, err = list(v1alpha1.ImageSelectorTerm{Family: "Ubuntu", OSVersion: "20.04", Version: "latest"})
	require.True(t, IsImageResolutionError(err))
	require.ErrorContains(t, err, "22.04, 24.04, 26.04")
}
