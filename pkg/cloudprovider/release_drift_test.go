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

package cloudprovider

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestImageReleaseRefreshDoesNotIntroduceDrift(t *testing.T) {
	const pinnedImage = "projects/test/global/images/pinned-ubuntu"
	claim := &karpv1.NodeClaim{}
	claim.Status.ImageID = pinnedImage
	class := &v1alpha1.GCENodeClass{Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{{
		SourceImage:  pinnedImage,
		Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}}},
	}}}}
	class.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
	for _, release := range []string{"", "1.35.8-gke.1796000", "1.36.4-gke.2046003"} {
		class.Status.Images[0].KubernetesVersion = release
		require.Empty(t, (&CloudProvider{}).isImageDrifted(claim, class), "missing or refreshed release alone must not replace a node")
	}
}
