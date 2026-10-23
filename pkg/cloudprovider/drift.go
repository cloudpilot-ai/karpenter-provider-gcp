/*
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
	"context"
	"strings"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
)

const (
	NodeClassDrift cloudprovider.DriftReason = "NodeClassDrift"
	ImageDrift     cloudprovider.DriftReason = "ImageDrift"
)

func (c *CloudProvider) isNodeClassDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim, _ *karpv1.NodePool, nodeClass *v1alpha1.GCENodeClass) cloudprovider.DriftReason {
	if drifted := c.areStaticFieldsDrifted(nodeClaim, nodeClass); drifted != "" {
		log.FromContext(ctx).Info("nodeclass drifted", "drifted", drifted)
		return drifted
	}

	if drifted := c.isImageDrifted(nodeClaim, nodeClass); drifted != "" {
		log.FromContext(ctx).Info("image drifted", "drifted", drifted)
		return drifted
	}

	return ""
}

func (c *CloudProvider) isImageDrifted(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass) cloudprovider.DriftReason {
	for _, im := range nodeClass.Status.Images {
		if strings.HasSuffix(nodeClaim.Status.ImageID, im.SourceImage) {
			return ""
		}
	}

	// A partial image set must not drift nodes without a compatible replacement.
	if !nodeClass.StatusConditions().IsTrue(v1alpha1.ConditionTypeImagesReady) {
		return ""
	}
	if _, ok := imagefamily.FindImage(nodeClass.Status.Images, imageReplacementRequirements(nodeClaim)); !ok {
		return ""
	}
	return ImageDrift
}

// Actual labels take precedence over broad spec requirements. A known architecture with no
// GPU-count label identifies a CPU node, even if the spec also permits GPU shapes.
func imageReplacementRequirements(nodeClaim *karpv1.NodeClaim) scheduling.Requirements {
	specRequirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	requirements := scheduling.NewRequirements()
	if value, ok := nodeClaim.Labels[corev1.LabelArchStable]; ok {
		requirements.Add(scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, value))
	} else if requirement, ok := specRequirements[corev1.LabelArchStable]; ok {
		requirements.Add(requirement)
	}
	gpuCount, hasGPUCountLabel := nodeClaim.Labels[v1alpha1.LabelInstanceGPUCount]
	_, hasArchLabel := nodeClaim.Labels[corev1.LabelArchStable]
	switch {
	case hasGPUCountLabel:
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpIn, gpuCount))
	case hasArchLabel:
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist))
	default:
		if requirement, ok := specRequirements[v1alpha1.LabelInstanceGPUCount]; ok {
			requirements.Add(requirement)
		}
	}
	return requirements
}

func (c *CloudProvider) areStaticFieldsDrifted(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass) cloudprovider.DriftReason {
	nodeClassHash, foundNodeClassHash := nodeClass.Annotations[v1alpha1.AnnotationGCENodeClassHash]
	nodeClassHashVersion, foundNodeClassHashVersion := nodeClass.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion]
	nodeClaimHash, foundNodeClaimHash := nodeClaim.Annotations[v1alpha1.AnnotationGCENodeClassHash]
	nodeClaimHashVersion, foundNodeClaimHashVersion := nodeClaim.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion]

	if !foundNodeClassHash || !foundNodeClaimHash || !foundNodeClassHashVersion || !foundNodeClaimHashVersion {
		return ""
	}
	// validate that the hash version for the GCENodeClass is the same as the NodeClaim before evaluating for static drift
	if nodeClassHashVersion != nodeClaimHashVersion {
		return ""
	}
	return lo.Ternary(nodeClassHash != nodeClaimHash, NodeClassDrift, "")
}
