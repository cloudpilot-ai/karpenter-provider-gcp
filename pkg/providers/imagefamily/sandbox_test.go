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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	v1 "k8s.io/api/core/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
)

type publicInstaller struct{}

func (publicInstaller) ResolveInstaller(_ context.Context, label, arch string) (*v1alpha1.SandboxInstaller, error) {
	version, err := sandbox.InstallerVersion(label)
	if err != nil {
		return nil, err
	}
	return &v1alpha1.SandboxInstaller{Version: version, Digest: "sha256:" + strings.Repeat("a", 64)}, nil
}
func sandboxClass(term v1alpha1.ImageSelectorTerm) *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SandboxConfig: &v1alpha1.SandboxConfig{Type: v1alpha1.SandboxTypeGVISOR}, ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{term}}}
}
func publicCOS(name, arch string) *compute.Image {
	return &compute.Image{Name: name, Status: "READY", Architecture: arch, Labels: map[string]string{"gvisor-version": "20260323-0_rc03"}}
}

func TestSandboxCatalogExactBuildRolesAndPins(t *testing.T) {
	cpu := "gke-1356-gke1250001-cos-125-19216-395-109-c-pre"
	gpu := "gke-1356-gke1250001-cos-125-19216-220-150-pre-gv-nvda"
	images := []*compute.Image{
		publicCOS("gke-1356-gke9999999-cos-125-19216-395-109-pre-gv-nvda", OSArchitectureX86),
		publicCOS(cpu, OSArchitectureX86),
		publicCOS(strings.Replace(cpu, "-cos-", "-cos-arm64-", 1), OSArchitectureARM),
		publicCOS(gpu, OSArchitectureX86),
		publicCOS(strings.Replace(gpu, "-cos-", "-cos-arm64-", 1), OSArchitectureARM),
		publicCOS(strings.Replace(cpu, "-c-pre", "-c-nvda", 1), OSArchitectureX86),
	}
	srv := cosImageServer(t, images)
	defer srv.Close()
	stub := &stubGKEProvider{serverConfig: &containerv1.ServerConfig{Channels: []*containerv1.ReleaseChannelConfig{{Channel: "STABLE", DefaultVersion: "1.35.6-gke.1250001"}}}}
	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "1.35.6-gke.1250001"}, stub)
	p.installerResolver = publicInstaller{}
	// Channel resolution must keep both CPU and GPU variants on the exact build.
	nc := sandboxClass(v1alpha1.ImageSelectorTerm{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: v1alpha1.ImageChannelStable})
	got, err := p.List(context.Background(), nc)
	require.NoError(t, err)
	require.Len(t, got, 4)
	gpuCount := 0
	for _, im := range got {
		require.NotNil(t, im.SandboxInstaller)
		require.Equal(t, "20260323.0_RC03", im.SandboxInstaller.Version)
		require.NotContains(t, im.SourceImage, "9999999")
		if im.Requirements.Get(v1alpha1.LabelInstanceGPUCount).Operator() == v1.NodeSelectorOpExists {
			gpuCount++
			require.Contains(t, im.SourceImage, "220-150-pre-gv-nvda")
		}
	}
	require.Equal(t, 2, gpuCount)
	missingBuild := sandboxClass(v1alpha1.ImageSelectorTerm{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: v1alpha1.ImageChannelStable})
	stub.serverConfig.Channels[0].DefaultVersion = "1.35.6-gke.8888888"
	// A separate provider avoids reusing the already resolved channel snapshot.
	next := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "1.35.6-gke.1250001"}, stub)
	next.installerResolver = publicInstaller{}
	_, err = next.List(context.Background(), missingBuild)
	require.ErrorContains(t, err, "8888888")

	pinned := sandboxClass(v1alpha1.ImageSelectorTerm{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Version: "125.19216.395.109"})
	got, err = p.List(context.Background(), pinned)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, im := range got {
		require.Equal(t, v1.NodeSelectorOpDoesNotExist, im.Requirements.Get(v1alpha1.LabelInstanceGPUCount).Operator())
	}
}

func TestSandboxExplicitIDProvenanceAndRole(t *testing.T) {
	name := "gke-1356-gke1250001-cos-arm64-125-19216-220-150-c-gvisor-nvda"
	for _, tt := range []struct {
		name, project, arch, label, state string
		deprecated                        bool
		valid                             bool
	}{
		{name, "gke-node-images", OSArchitectureARM, "20260323-0_rc03", "READY", false, true},
		{name, "copied-images", OSArchitectureARM, "20260323-0_rc03", "READY", false, false},
		{name, "gke-node-images", OSArchitectureX86, "20260323-0_rc03", "READY", false, false},
		{name, "gke-node-images", OSArchitectureARM, "", "READY", false, false},
		{name, "gke-node-images", OSArchitectureARM, "20260323-0_rc03", "PENDING", false, false},
		{name, "gke-node-images", OSArchitectureARM, "20260323-0_rc03", "READY", true, false},
		{"gke-1356-gke1250001-ubuntu-2404-amd64-v20260416", "gke-node-images", OSArchitectureX86, "20260323-0_rc03", "READY", false, false},
		{"gke-1356-gke1250001-cos-125-19216-395-109-c-nvda", "gke-node-images", OSArchitectureX86, "20260323-0_rc03", "READY", false, false},
	} {
		t.Run(tt.project+"/"+tt.name+"/"+tt.arch+"/"+tt.label+"/"+tt.state, func(t *testing.T) {
			img := publicCOS(tt.name, tt.arch)
			img.Status = tt.state
			img.Labels["gvisor-version"] = tt.label
			if tt.deprecated {
				img.Deprecated = &compute.DeprecationStatus{State: "DEPRECATED"}
			}
			srv := cosImageServer(t, []*compute.Image{img})
			defer srv.Close()
			p := NewDefaultProvider(buildComputeService(t, srv), nil, nil)
			p.installerResolver = publicInstaller{}
			got, err := p.List(context.Background(), sandboxClass(v1alpha1.ImageSelectorTerm{ID: "projects/" + tt.project + "/global/images/" + tt.name}))
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, "arm64", got[0].Requirements.Get(v1.LabelArchStable).Any())
			require.Equal(t, "projects/gke-node-images/global/images/"+tt.name, got[0].SourceImage)
			require.Equal(t, v1.NodeSelectorOpExists, got[0].Requirements.Get(v1alpha1.LabelInstanceGPUCount).Operator())
		})
	}
}

func TestSandboxAndOrdinaryImageCachesAreSeparate(t *testing.T) {
	name := "gke-1356-gke1250001-cos-125-19216-395-109-c-pre"
	srv := cosImageServer(t, []*compute.Image{publicCOS(name, OSArchitectureX86)})
	defer srv.Close()
	p := NewDefaultProvider(buildComputeService(t, srv), nil, nil)
	p.installerResolver = publicInstaller{}
	nc := sandboxClass(v1alpha1.ImageSelectorTerm{ID: "projects/gke-node-images/global/images/" + name})
	for _, enabled := range []bool{false, true, false} {
		if enabled {
			nc.Spec.SandboxConfig = &v1alpha1.SandboxConfig{Type: v1alpha1.SandboxTypeGVISOR}
		} else {
			nc.Spec.SandboxConfig = nil
		}
		got, err := p.List(context.Background(), nc)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, enabled, got[0].SandboxInstaller != nil)
		if enabled {
			require.Equal(t, v1.NodeSelectorOpDoesNotExist, got[0].Requirements.Get(v1alpha1.LabelInstanceGPUCount).Operator())
		}
	}
}

func TestSandboxImageReleaseUsesExactSelectedPublicBuild(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"projects/gke-node-images/global/images/gke-1340-gke1713000-cos-125-1-2-3-c-pre", "1.34.0-gke.1713000"},
		{"projects/gke-node-images/global/images/gke-13011-gke1093000-cos-125-1-2-3-pre-gv-nvda", "1.30.11-gke.1093000"},
		{"projects/gke-node-images/global/images/gke-1356-gke1250001-cos-arm64-125-1-2-3-pre-gvisor-nvda", "1.35.6-gke.1250001"},
	} {
		got, err := SandboxImageRelease(tc.source)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
	for _, source := range []string{"projects/custom/global/images/gke-1356-gke1250001-cos-125-1-2-3-c-pre", "projects/gke-node-images/global/images/gke-1356-gke1250001-ubuntu-2404-1-2-3-c-pre", "projects/gke-node-images/global/images/gke-1356-gke1250001-cos-125-1-2-3-pre-nvda"} {
		_, err := SandboxImageRelease(source)
		require.Error(t, err)
	}
}

func TestSandboxCatalogIgnoresUnusedOlderRevisionInstaller(t *testing.T) {
	cpu := publicCOS("gke-1356-gke1250001-cos-125-19216-395-109-c-pre", OSArchitectureX86)
	preferred := publicCOS("gke-1356-gke1250001-cos-125-19216-220-150-pre-gv-nvda", OSArchitectureX86)
	older := publicCOS("gke-1356-gke1250001-cos-125-19216-104-126-pre-gvisor-nvda", OSArchitectureX86)
	older.Labels["gvisor-version"] = "20250811-0_rc05"
	srv := cosImageServer(t, []*compute.Image{cpu, preferred, older})
	defer srv.Close()
	p := NewDefaultProvider(buildComputeService(t, srv), nil, nil)
	p.installerResolver = missingOldInstaller{}
	got, err := p.List(context.Background(), sandboxClass(v1alpha1.ImageSelectorTerm{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Version: "1.35.6-gke.1250001"}))
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "projects/gke-node-images/global/images/"+preferred.Name, got[1].SourceImage)
	// Explicit requests remain strict even for an otherwise unselected revision.
	_, err = p.List(context.Background(), sandboxClass(v1alpha1.ImageSelectorTerm{ID: "projects/gke-node-images/global/images/" + older.Name}))
	require.ErrorContains(t, err, "installer unavailable")
	// The preferred variant cannot silently fall back to an older installer.
	preferred.Labels["gvisor-version"] = "20250811-0_rc05"
	older.Labels["gvisor-version"] = "20260323-0_rc03"
	_, err = p.resolveSandboxCatalog(context.Background(), "1.35.6-gke.1250001")
	require.ErrorContains(t, err, "installer unavailable")
}

type missingOldInstaller struct{}

func (missingOldInstaller) ResolveInstaller(ctx context.Context, label, arch string) (*v1alpha1.SandboxInstaller, error) {
	if label == "20250811-0_rc05" {
		return nil, fmt.Errorf("installer unavailable")
	}
	return publicInstaller{}.ResolveInstaller(ctx, label, arch)
}
