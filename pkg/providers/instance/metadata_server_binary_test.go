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

package instance

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/metadata"
)

// Metadata/disk policy tests supply an already resolved launch; fetching and
// selection are exercised separately rather than through every policy fixture.
func testLaunchImage(class *v1alpha1.GCENodeClass) launchImage {
	image := "projects/gke-node-images/global/images/gke-cos"
	if len(class.Status.Images) > 0 {
		image = class.Status.Images[0].SourceImage
	}
	const version = "1.36.4-gke.2046003"
	const base = "https://storage.googleapis.com/gke-release/kubernetes/release/v" + version + "/"
	return launchImage{SourceImage: image, Bootstrap: metadata.BootstrapRelease{
		Version:          version,
		ServerBinaryURL:  base + "kubernetes-server-linux-amd64.tar.gz",
		ServerBinaryHash: strings.Repeat("a", 128),
		ManifestsURL:     base + "kubernetes-manifests.tar.gz",
		ManifestsHash:    strings.Repeat("b", 128),
		ConfigureScript:  "target configure script",
		UserData:         "target node entrypoint",
	}}
}

type bootstrapFunc func(context.Context, string, string) (metadata.BootstrapRelease, error)

func (f bootstrapFunc) Get(ctx context.Context, version, arch string) (metadata.BootstrapRelease, error) {
	return f(ctx, version, arch)
}

func TestResolveLaunchImageUsesSelectedStatusReleaseAndArchitecture(t *testing.T) {
	class := &v1alpha1.GCENodeClass{Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{
		{SourceImage: "projects/test/global/images/amd64-pin", KubernetesVersion: "1.35.8-gke.1796000", Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}}}},
		{SourceImage: "projects/test/global/images/arm64-pin", KubernetesVersion: "1.36.4-gke.2046003", Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"arm64"}}}},
	}}}
	provider := &DefaultProvider{bootstrapProvider: bootstrapFunc(func(_ context.Context, version, arch string) (metadata.BootstrapRelease, error) {
		require.Equal(t, "1.36.4-gke.2046003", version)
		require.Equal(t, "arm64", arch)
		return metadata.BootstrapRelease{Version: version}, nil
	})}
	instanceType := &cloudprovider.InstanceType{Requirements: scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64"),
	)}
	selected, err := provider.resolveLaunchImage(context.Background(), class, instanceType)
	require.NoError(t, err)
	require.Equal(t, "projects/test/global/images/arm64-pin", selected.SourceImage)
	require.Equal(t, "1.36.4-gke.2046003", selected.Bootstrap.Version)
}

func TestResolveLaunchImageWaitsForOldStatusWithoutGuessingRelease(t *testing.T) {
	class := &v1alpha1.GCENodeClass{Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{{SourceImage: "projects/test/global/images/pinned"}}}}
	_, err := (&DefaultProvider{}).resolveLaunchImage(context.Background(), class, makeNonGPUIT())
	require.ErrorContains(t, err, "update CRDs")
	require.False(t, cloudprovider.IsInsufficientCapacityError(err))
}

func TestResolveLaunchImageDoesNotFallBackOnArtifactFailure(t *testing.T) {
	class := &v1alpha1.GCENodeClass{Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{{SourceImage: "projects/test/global/images/pinned", KubernetesVersion: "1.36.4-gke.2046003"}}}}
	failure := errors.New("artifact service unavailable")
	provider := &DefaultProvider{bootstrapProvider: bootstrapFunc(func(context.Context, string, string) (metadata.BootstrapRelease, error) {
		return metadata.BootstrapRelease{}, failure
	})}
	_, err := provider.resolveLaunchImage(context.Background(), class, makeNonGPUIT())
	require.ErrorIs(t, err, failure)
	require.False(t, cloudprovider.IsInsufficientCapacityError(err))
}
