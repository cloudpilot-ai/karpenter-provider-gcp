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

func TestResolveLatestUbuntuImage_PicksNewestNonDeprecated(t *testing.T) {
	images := []*compute.Image{
		{
			Name:              "ubuntu-gke-2404-1-35-amd64-v20260420",
			CreationTimestamp: "2026-04-20T00:00:00Z",
		},
		{
			Name:              "ubuntu-gke-2404-1-35-amd64-v20260401",
			CreationTimestamp: "2026-04-01T00:00:00Z",
		},
		{
			// deprecated — must be excluded
			Name:              "ubuntu-gke-2404-1-35-amd64-v20260301",
			CreationTimestamp: "2026-03-01T00:00:00Z",
			Deprecated:        &compute.DeprecationStatus{State: "DEPRECATED"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: images}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &Ubuntu{
		computeService:  buildComputeService(t, srv),
		versionProvider: &fakeVersionProvider{version: "v1.35.1"},
	}

	got, err := p.resolveLatestUbuntuImage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20260420", got)
}

func TestResolveLatestUbuntuImage_ExcludesNonUsable(t *testing.T) {
	images := []*compute.Image{
		// excluded: arm64 (no -amd64-)
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260420", CreationTimestamp: "2026-04-20T00:00:00Z"},
		// excluded: test variant
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260419-test", CreationTimestamp: "2026-04-19T00:00:00Z"},
		// excluded: cgroupsv1
		{Name: "ubuntu-gke-2404-1-35-amd64-cgroupsv1-v20260418", CreationTimestamp: "2026-04-18T00:00:00Z"},
		// excluded: tpu
		{Name: "ubuntu-gke-2404-1-35-amd64-tpu-v20260417", CreationTimestamp: "2026-04-17T00:00:00Z"},
		// excluded: linux64k
		{Name: "ubuntu-gke-2404-1-35-amd64-linux64k-v20260416", CreationTimestamp: "2026-04-16T00:00:00Z"},
		// excluded: obsolete
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260301", CreationTimestamp: "2026-03-01T00:00:00Z", Deprecated: &compute.DeprecationStatus{State: "OBSOLETE"}},
		// only valid candidate
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260401", CreationTimestamp: "2026-04-01T00:00:00Z"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &compute.ImageList{Items: images}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &Ubuntu{
		computeService:  buildComputeService(t, srv),
		versionProvider: &fakeVersionProvider{version: "v1.35.1"},
	}

	got, err := p.resolveLatestUbuntuImage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20260401", got)
}

func TestResolveImages_Ubuntu_DrivesArm64Variant(t *testing.T) {
	p := &Ubuntu{}
	sourceImage := "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20260420"

	imgs := p.resolveImages(sourceImage)
	require.Len(t, imgs, 2)

	var srcs []string
	for _, img := range imgs {
		srcs = append(srcs, img.SourceImage)
	}

	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-amd64-v20260420")
	require.Contains(t, srcs, "projects/ubuntu-os-gke-cloud/global/images/ubuntu-gke-2404-1-35-arm64-v20260420")
}

func TestBuildImageFilter_Ubuntu_UsesVersionPrefix(t *testing.T) {
	p := &Ubuntu{versionProvider: &fakeVersionProvider{version: "v1.35.1"}}
	got := p.buildImageFilter(context.Background())
	require.Equal(t, `name=ubuntu-gke-2404-1-35*`, got)
}

func TestBuildImageFilter_Ubuntu_FallsBackOnNilProvider(t *testing.T) {
	p := &Ubuntu{}
	got := p.buildImageFilter(context.Background())
	require.Equal(t, `name=ubuntu-gke-2404*`, got)
}

func TestIsUsableUbuntuImage(t *testing.T) {
	usable := []*compute.Image{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420"},
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
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260420-test"},
		{Name: "ubuntu-gke-2404-1-35-amd64-cgroupsv1-v20260420"},
		{Name: "ubuntu-gke-2404-1-35-amd64-linux64k-v20260420"},
		{Name: "ubuntu-gke-2404-1-35-amd64-tpu-v20260420"},
	}
	for _, img := range unusable {
		require.False(t, isUsableUbuntuImage(img), "expected %q to be unusable", img.Name)
	}
}
