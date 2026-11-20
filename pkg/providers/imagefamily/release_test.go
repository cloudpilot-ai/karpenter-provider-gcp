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
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

type changingVersionProvider struct {
	versions []string
	calls    int
}

func (p *changingVersionProvider) Get(context.Context) (string, error) {
	index := min(p.calls, len(p.versions)-1)
	p.calls++
	return p.versions[index], nil
}

func TestListCOSReleaseFollowsEachSelectedVariantNotMasterBuild(t *testing.T) {
	const cpu = "gke-13510-gke1153000-cos-125-19216-801-52-c-pre"
	const arm = "gke-13510-gke1123000-cos-arm64-125-19216-800-50-c-pre"
	srv := cosImageServer(t, []*compute.Image{
		{Name: cpu, Status: "READY"}, {Name: arm, Status: "READY"},
	})
	defer srv.Close()
	provider := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.10-gke.9999999"}, nil)
	images, err := provider.List(context.Background(), cosLatestNodeClass())
	require.NoError(t, err)
	require.Len(t, images, 2)
	require.Equal(t, "projects/gke-node-images/global/images/"+cpu, images[0].SourceImage)
	require.Equal(t, "1.35.10-gke.1153000", images[0].KubernetesVersion)
	require.Equal(t, "projects/gke-node-images/global/images/"+arm, images[1].SourceImage)
	require.Equal(t, "1.35.10-gke.1123000", images[1].KubernetesVersion)
}

func TestListExplicitImageReleasePolicyPreservesImagePin(t *testing.T) {
	for _, test := range []struct {
		name, project, image, want string
	}{
		{"official historical COS pin", "gke-node-images", "gke-13413-gke1088000-cos-125-19216-801-52-c-pre", "1.34.13-gke.1088000"},
		{"opaque custom pin", "test-project", "my-custom-image", "1.36.4-gke.2046003"},
		{"custom project GKE lookalike", "test-project", "gke-1358-gke1796000-cos-125-19216-801-52-c-pre", "1.36.4-gke.2046003"},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := &imageCatalog{pages: [][]*compute.Image{{{Name: test.image, Architecture: OSArchitectureX86, Status: "READY"}}}}
			srv := imageCatalogServer(t, catalog)
			defer srv.Close()
			version := &changingVersionProvider{versions: []string{"v1.36.4-gke.2046003"}}
			provider := NewDefaultProvider(buildComputeService(t, srv), version, nil)
			id := "projects/" + test.project + "/global/images/" + test.image
			images, err := provider.List(context.Background(), &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{ID: id}}}})
			require.NoError(t, err)
			require.Len(t, images, 1)
			require.Equal(t, id, images[0].SourceImage)
			require.Equal(t, test.want, images[0].KubernetesVersion)
			if test.project == "gke-node-images" {
				require.Zero(t, version.calls)
			} else {
				require.Equal(t, 1, version.calls)
			}
		})
	}
}

func TestListDoesNotGuessAmbiguousHistoricalOfficialCOSName(t *testing.T) {
	const name = "gke-198-gke123-cos-69-10895-52-0-c-pre"
	srv := imageCatalogServer(t, &imageCatalog{pages: [][]*compute.Image{{{Name: name, Architecture: OSArchitectureX86, Status: "READY"}}}})
	defer srv.Close()
	provider := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.36.4-gke.2046003"}, nil)
	_, err := provider.List(context.Background(), &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{ID: "projects/gke-node-images/global/images/" + name}}}})
	require.True(t, IsImageResolutionError(err))
}

func TestUbuntuResolutionAndCacheKeepOneVersionSnapshotWithImage(t *testing.T) {
	catalog := &imageCatalog{pages: [][]*compute.Image{{
		{Name: "ubuntu-gke-2404-1-35-amd64-v20260416", Status: "READY"},
		{Name: "ubuntu-gke-2404-1-35-arm64-v20260416", Status: "READY"},
	}}}
	srv := imageCatalogServer(t, catalog)
	defer srv.Close()
	version := &changingVersionProvider{versions: []string{"v1.35.8-gke.1796000", "v1.36.4-gke.2046003"}}
	provider := NewDefaultProvider(buildComputeService(t, srv), version, nil)
	class := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyUbuntu2404, Version: "v20260416"}}}}
	images, err := provider.List(context.Background(), class)
	require.NoError(t, err)
	require.Equal(t, 1, version.calls, "filter and bootstrap target must share one API-server snapshot")
	for _, image := range images {
		require.Equal(t, "1.35.8-gke.1796000", image.KubernetesVersion)
	}

	cached, err := provider.List(context.Background(), class)
	require.NoError(t, err)
	require.Equal(t, images, cached)
	require.Equal(t, 1, version.calls, "cached image must not be combined with a new master version")

	catalog.pages = [][]*compute.Image{{
		{Name: "ubuntu-gke-2404-1-36-amd64-v20260416", Status: "READY"},
		{Name: "ubuntu-gke-2404-1-36-arm64-v20260416", Status: "READY"},
	}}
	provider.cache.Flush()
	updated, err := provider.List(context.Background(), class)
	require.NoError(t, err)
	require.Equal(t, 2, version.calls)
	for _, image := range updated {
		require.Equal(t, "1.36.4-gke.2046003", image.KubernetesVersion)
		require.Contains(t, image.SourceImage, "-1-36-")
		require.Contains(t, image.SourceImage, "v20260416", "date pin remains unchanged")
	}
}
