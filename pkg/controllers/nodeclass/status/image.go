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

package status

import (
	"context"
	"sort"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
)

// Condition reasons published on ImagesReady.
const (
	// imageResolutionFailedReason marks a NodeClass selector that cannot resolve: either a
	// configuration error, e.g. an invalid version format or an unsupported family, or an Ubuntu
	// catalog discovery miss with no usable image for an architecture. The Ubuntu case can clear
	// without a configuration change once GCP publishes the image.
	imageResolutionFailedReason = "ImageResolutionFailed"
	// imageUnavailableReason marks a valid selector whose images are not published yet. The
	// previous images are cleared because no usable image exists for the selector right now, and
	// the resolution is retried shortly instead of waiting for another NodeClass event.
	imageUnavailableReason = "ImageUnavailable"
	// imageLookupFailedReason marks a Compute API failure while resolving images. The previous
	// images stay in status for diagnosis, but they are not usable for new launches.
	imageLookupFailedReason = "ImageLookupFailed"
	// imagesNotFoundReason marks a selector that resolved but matched no image.
	imagesNotFoundReason = "ImagesNotFound"
)

type Image struct {
	imageProvider imagefamily.Provider
}

func (i *Image) Reconcile(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (reconcile.Result, error) {
	images, err := i.imageProvider.List(ctx, nodeClass)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing images")
		switch {
		case imagefamily.IsImageResolutionError(err):
			nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, imageResolutionFailedReason, imageConditionMessage(err))
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		case imagefamily.IsImageUnavailableError(err):
			// A confirmed no-usable-image result is not a stale selector: clearing the previous
			// images keeps status from advertising a set Karpenter has already proven unusable.
			nodeClass.Status.Images = nil
			nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, imageUnavailableReason, imageConditionMessage(err))
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		default:
			// The Compute API may be transiently unavailable: publish a diagnostic condition but
			// return the error so controller-runtime backs off as usual.
			nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, imageLookupFailedReason, imageConditionMessage(err))
			return reconcile.Result{}, err
		}
	}

	if len(images) == 0 {
		nodeClass.Status.Images = nil
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, imagesNotFoundReason, "ImageSelector did not match any Images")
		return reconcile.Result{RequeueAfter: time.Minute}, nil
	}

	nodeClass.Status.Images = lo.Map(images, func(image imagefamily.Image, _ int) v1alpha1.Image {
		reqs := lo.Map(image.Requirements.NodeSelectorRequirements(), func(item karpv1.NodeSelectorRequirementWithMinValues, _ int) corev1.NodeSelectorRequirement {
			return corev1.NodeSelectorRequirement{Key: item.Key, Operator: item.Operator, Values: item.Values}
		})

		sort.Slice(reqs, func(i, j int) bool {
			if len(reqs[i].Key) != len(reqs[j].Key) {
				return len(reqs[i].Key) < len(reqs[j].Key)
			}
			return reqs[i].Key < reqs[j].Key
		})
		return v1alpha1.Image{
			SourceImage:  image.SourceImage,
			Requirements: reqs,
		}
	})
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)

	return reconcile.Result{RequeueAfter: 5 * time.Minute}, nil
}

// imageConditionMessage bounds a failure message to the 256-rune limit of a condition message.
func imageConditionMessage(err error) string {
	message := err.Error()
	if len([]rune(message)) > 256 {
		return string([]rune(message)[:256])
	}
	return message
}
