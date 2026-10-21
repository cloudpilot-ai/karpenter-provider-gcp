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

const (
	imageResolutionFailedReason = "ImageResolutionFailed"
	imageUnavailableReason      = "ImageUnavailable"
	imageLookupFailedReason     = "ImageLookupFailed"
	imagesNotFoundReason        = "ImagesNotFound"
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
			// Confirmed unavailability invalidates the previous image set.
			nodeClass.Status.Images = nil
			nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, imageUnavailableReason, imageConditionMessage(err))
			return reconcile.Result{RequeueAfter: time.Minute}, nil
		default:
			// Retain diagnostic status, but block launches and preserve infrastructure backoff.
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
