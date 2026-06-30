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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestKubeProxyReadinessLabelIsNotWellKnown(t *testing.T) {
	require.False(t, karpv1.WellKnownLabels.Has(LabelGKEReadinessKubeProxyReady))
}

func TestImageFamily_FromSpecImageFamily(t *testing.T) {
	cos := ImageFamilyContainerOptimizedOS
	nc := &GCENodeClass{Spec: GCENodeClassSpec{ImageFamily: &cos}}
	require.Equal(t, ImageFamilyContainerOptimizedOS, nc.ImageFamily())
}

func TestImageFamily_FromAlias(t *testing.T) {
	nc := &GCENodeClass{Spec: GCENodeClassSpec{
		ImageSelectorTerms: []ImageSelectorTerm{{Alias: "ContainerOptimizedOS@latest"}},
	}}
	require.Equal(t, ImageFamilyContainerOptimizedOS, nc.ImageFamily())
}

func TestImageFamily_FromFamilyFieldCOS(t *testing.T) {
	nc := &GCENodeClass{Spec: GCENodeClassSpec{
		ImageSelectorTerms: []ImageSelectorTerm{{Family: ImageFamilyContainerOptimizedOS, Channel: ImageChannelStable}},
	}}
	require.Equal(t, ImageFamilyContainerOptimizedOS, nc.ImageFamily())
}

func TestImageFamily_FromFamilyFieldUbuntu2404(t *testing.T) {
	nc := &GCENodeClass{Spec: GCENodeClassSpec{
		ImageSelectorTerms: []ImageSelectorTerm{{Family: ImageFamilyUbuntu2404, Version: "latest"}},
	}}
	// Must return ImageFamilyUbuntu (not Ubuntu2404) — downstream PatchKubeEnvForOSType
	// switches on only two known values: Ubuntu and ContainerOptimizedOS.
	require.Equal(t, ImageFamilyUbuntu, nc.ImageFamily())
}

func TestImageFamily_FromFamilyFieldUbuntu2204(t *testing.T) {
	nc := &GCENodeClass{Spec: GCENodeClassSpec{
		ImageSelectorTerms: []ImageSelectorTerm{{Family: ImageFamilyUbuntu2204, Version: "latest"}},
	}}
	require.Equal(t, ImageFamilyUbuntu, nc.ImageFamily())
}

func TestImageFamily_NoTerms_ReturnsCustom(t *testing.T) {
	nc := &GCENodeClass{Spec: GCENodeClassSpec{
		ImageSelectorTerms: []ImageSelectorTerm{{ID: "projects/p/global/images/some-image"}},
	}}
	require.Equal(t, ImageFamilyCustom, nc.ImageFamily())
}
