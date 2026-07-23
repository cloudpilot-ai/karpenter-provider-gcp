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
)

func TestSelectImage_Ubuntu_PicksNewestNonDeprecatedAmd64(t *testing.T) {
	images := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420", CreationTimestamp: "2026-04-20T00:00:00Z"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260401", CreationTimestamp: "2026-04-01T00:00:00Z"},
		// deprecated — must be excluded
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260301", CreationTimestamp: "2026-03-01T00:00:00Z",
			Deprecated: &compute.DeprecationStatus{State: "DEPRECATED"}},
	}

	p := &Ubuntu{release: "2404"}
	got, ok := p.selectImage(images, OSArchAMD64Requirement, "latest")
	require.True(t, ok)
	require.Equal(t, "ubuntu-gke-2404-1-35-amd64-v20260420", got)
}

func TestSelectImage_Ubuntu_ExcludesNonCleanAmd64(t *testing.T) {
	images := []*compute.Image{
		// excluded: arm64 (different architecture)
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260420", CreationTimestamp: "2026-04-20T00:00:00Z"},
		// excluded: variants, in either infix or trailing-suffix position
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260419-test", CreationTimestamp: "2026-04-19T00:00:00Z"},
		{Name: "ubuntu-gke-2404-1-35-amd64-cgroupsv1-v20260418", CreationTimestamp: "2026-04-18T00:00:00Z"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260418-tpu", CreationTimestamp: "2026-04-18T12:00:00Z"},
		{Name: "ubuntu-gke-2404-1-35-amd64-linux64k-v20260416", CreationTimestamp: "2026-04-16T00:00:00Z"},
		// excluded: obsolete
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260301", CreationTimestamp: "2026-03-01T00:00:00Z", Deprecated: &compute.DeprecationStatus{State: "OBSOLETE"}},
		// only valid candidate
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260401", CreationTimestamp: "2026-04-01T00:00:00Z"},
	}

	p := &Ubuntu{release: "2404"}
	got, ok := p.selectImage(images, OSArchAMD64Requirement, "latest")
	require.True(t, ok)
	require.Equal(t, "ubuntu-gke-2404-1-35-amd64-v20260401", got)
}

func TestBuildImageFilter_Ubuntu_UsesVersionPrefix(t *testing.T) {
	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.35.1"}, release: "2404"}
	got := p.buildImageFilter(context.Background())
	require.Equal(t, `name=ubuntu-gke-2404-1-35*`, got)
}

func TestBuildImageFilter_Ubuntu_FallsBackOnNilProvider(t *testing.T) {
	p := &Ubuntu{release: "2404"}
	got := p.buildImageFilter(context.Background())
	require.Equal(t, `name=ubuntu-gke-2404*`, got)
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

	p := &Ubuntu{
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

	p := &Ubuntu{
		computeService: buildComputeService(t, srv),
		release:        "2404",
	}

	_, err := p.ResolveImages(context.Background(), "v20260416")
	require.Error(t, err)
	require.True(t, IsImageResolutionError(err), "expected an imageResolutionError, got %v", err)
}

func TestResolveImages_Ubuntu_PinnedVersion_InvalidFormat(t *testing.T) {
	p := &Ubuntu{}
	for _, version := range []string{"20260416", "v202604161", "v2026041", "vABCDEFGH", "v20260416-extra"} {
		_, err := p.ResolveImages(context.Background(), version)
		require.Errorf(t, err, "expected error for version %q", version)
		require.Contains(t, err.Error(), "invalid Ubuntu version", "version %q", version)
	}
}

// TestIsUsableUbuntuImage asserts the 2404 amd64 predicate: it accepts a clean amd64 image
// (including the -v<date>a letter suffix) and rejects deprecated images, the other arch, and
// every variant build, whether the variant token is a trailing suffix (the real GCP form) or
// an infix.
func TestIsUsableUbuntuImage(t *testing.T) {
	usable := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20251218a"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420", Deprecated: &compute.DeprecationStatus{State: ""}},
	}
	for _, img := range usable {
		require.True(t, isUsableUbuntuImage(img), "expected %q to be usable", img.Name)
	}

	unusable := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420", Deprecated: &compute.DeprecationStatus{State: "DEPRECATED"}},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420", Deprecated: &compute.DeprecationStatus{State: "OBSOLETE"}},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420", Deprecated: &compute.DeprecationStatus{State: "DELETED"}},
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260420"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-tpu"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-cgroupsv1"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-linux64k"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-test"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-devel"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-tpu-test"},
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260722-custom-test"},
		{Name: "ubuntu-gke-2404-1-35-amd64-cgroupsv1-v20260420"},
		{Name: "ubuntu-gke-2404-1-35-amd64-tpu-v20260420"},
	}
	for _, img := range unusable {
		require.False(t, isUsableUbuntuImage(img), "expected %q to be unusable", img.Name)
	}
}

// TestIsUsableUbuntu2204Image asserts the 2204 amd64 predicate, where amd64 names carry no arch token.
func TestIsUsableUbuntu2204Image(t *testing.T) {
	usable := []*compute.Image{
		{Name: "ubuntu-gke-2204-1-34-v20260420"},
		{Name: "ubuntu-gke-2204-1-34-v20251218a"},
		{Name: "ubuntu-gke-2204-1-34-v20260420", Deprecated: &compute.DeprecationStatus{State: ""}},
	}
	for _, img := range usable {
		require.True(t, isUsableUbuntu2204Image(img), "expected %q to be usable", img.Name)
	}

	unusable := []*compute.Image{
		{Name: "ubuntu-gke-2204-1-34-v20260420", Deprecated: &compute.DeprecationStatus{State: "DEPRECATED"}},
		{Name: "ubuntu-gke-2204-1-34-v20260420", Deprecated: &compute.DeprecationStatus{State: "OBSOLETE"}},
		{Name: "ubuntu-gke-2204-1-34-arm64-v20260420"},
		{Name: "ubuntu-gke-2204-1-34-v20260722-tpu"},
		{Name: "ubuntu-gke-2204-1-34-v20260722-cgroupsv1"},
		{Name: "ubuntu-gke-2204-1-34-v20260722-linux64k"},
		{Name: "ubuntu-gke-2204-1-34-v20260722-test"},
		{Name: "ubuntu-gke-2204-1-34-v20260722-devel"},
		{Name: "ubuntu-gke-2204-1-34-v20260722-cgroupsv1-devel"},
	}
	for _, img := range unusable {
		require.False(t, isUsableUbuntu2204Image(img), "expected %q to be unusable", img.Name)
	}
}

// TestIsUsableUbuntuArm64Image covers the arm64 predicates added for independent arm64 resolution.
func TestIsUsableUbuntuArm64Image(t *testing.T) {
	require.True(t, isUsableUbuntuArm64Image(&compute.Image{Name: "ubuntu-gke-2404-1-35-arm64-v20260420"}))
	require.True(t, isUsableUbuntuArm64Image(&compute.Image{Name: "ubuntu-gke-2404-1-35-arm64-v20251218a"}))
	require.True(t, isUsableUbuntu2204Arm64Image(&compute.Image{Name: "ubuntu-gke-2204-1-34-arm64-v20260420"}))

	for _, img := range []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420"},          // amd64, not arm64
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260722-linux64k"}, // variant
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260420", Deprecated: &compute.DeprecationStatus{State: "DEPRECATED"}},
	} {
		require.False(t, isUsableUbuntuArm64Image(img), "expected %q to be unusable", img.Name)
	}
	require.False(t, isUsableUbuntu2204Arm64Image(&compute.Image{Name: "ubuntu-gke-2204-1-34-v20260420"}))
}

// Regression coverage for arm64 / trailing-variant-suffix resolution: each architecture is
// resolved independently to a real image, and a missing architecture fails resolution.

func imageListServer(t *testing.T, images []*compute.Image) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: images}
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

	p := &Ubuntu{
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

	p := &Ubuntu{
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

	p := &Ubuntu{
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

	p := &Ubuntu{
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

func TestBuildImageFilter_Ubuntu2204_UsesVersionPrefix(t *testing.T) {
	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.34.7"}, release: "2204"}
	got := p.buildImageFilter(context.Background())
	require.Equal(t, `name=ubuntu-gke-2204-1-34*`, got)
}

func TestBuildImageFilter_Ubuntu2204_FallsBackOnNilProvider(t *testing.T) {
	p := &Ubuntu{release: "2204"}
	got := p.buildImageFilter(context.Background())
	require.Equal(t, `name=ubuntu-gke-2204*`, got)
}

func TestSelectImage_Ubuntu2204_PicksAmd64NotArm64(t *testing.T) {
	images := []*compute.Image{
		// arm64 — different architecture, must not be selected for amd64
		{Name: "ubuntu-gke-2204-1-34-arm64-v20260420", CreationTimestamp: "2026-04-20T00:00:00Z"},
		// valid amd64 (no arch token in 2204)
		{Name: "ubuntu-gke-2204-1-34-v20260401", CreationTimestamp: "2026-04-01T00:00:00Z"},
	}

	p := &Ubuntu{release: "2204"}
	got, ok := p.selectImage(images, OSArchAMD64Requirement, "latest")
	require.True(t, ok)
	require.Equal(t, "ubuntu-gke-2204-1-34-v20260401", got)
}
