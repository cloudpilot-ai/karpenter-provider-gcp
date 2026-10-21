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
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// ImageCompatibleWithRequirements reports whether a resolved image can back a node whose
// requirements are the given requirements. Scheduling eligibility, new launches, boot disk
// selection, and image drift share this comparison so an image is never selected where another
// check would reject it. The direction and the undefined-label policy match boot disk rendering:
// every requirement the node declares must accept the image's own requirements, and well-known
// labels the image does not declare stay unconstrained.
func ImageCompatibleWithRequirements(image v1alpha1.Image, requirements scheduling.Requirements) bool {
	return scheduling.NewNodeSelectorRequirements(image.Requirements...).
		Compatible(requirements, scheduling.AllowUndefinedWellKnownLabels) == nil
}

// ImagesReadyForLaunch reports whether the NodeClass currently publishes images a new node may
// boot. The readiness condition, not the presence of images, is authoritative: a failed refresh can
// leave an earlier selector's images in status for diagnosis, and those must never launch.
func ImagesReadyForLaunch(nodeClass *v1alpha1.GCENodeClass) bool {
	return nodeClass.StatusConditions().IsTrue(v1alpha1.ConditionTypeImagesReady) && len(nodeClass.Status.Images) > 0
}
