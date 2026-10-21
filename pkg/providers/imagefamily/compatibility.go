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
	"github.com/samber/lo"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// FindImage returns the first resolved image compatible with the node's requirements.
func FindImage(images []v1alpha1.Image, requirements scheduling.Requirements) (v1alpha1.Image, bool) {
	for _, image := range images {
		if scheduling.NewNodeSelectorRequirements(image.Requirements...).Compatible(requirements, scheduling.AllowUndefinedWellKnownLabels) == nil {
			return image, true
		}
	}
	return v1alpha1.Image{}, false
}

// FilterInstanceTypes keeps launchable types without modifying the catalog. Retained status
// after a failed refresh is diagnostic only; it must not authorize new launches.
func FilterInstanceTypes(nodeClass *v1alpha1.GCENodeClass, instanceTypes []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	if !nodeClass.StatusConditions().IsTrue(v1alpha1.ConditionTypeImagesReady) || len(nodeClass.Status.Images) == 0 {
		return nil
	}
	return lo.Filter(instanceTypes, func(instanceType *cloudprovider.InstanceType, _ int) bool {
		_, ok := FindImage(nodeClass.Status.Images, instanceType.Requirements)
		return ok
	})
}
