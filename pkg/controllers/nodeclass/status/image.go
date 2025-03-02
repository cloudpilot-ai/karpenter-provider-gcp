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

type Image struct {
	imageProvider imagefamily.Provider
}

func (i *Image) Reconcile(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (reconcile.Result, error) {
	images, err := i.imageProvider.List(ctx, nodeClass)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing images")
		return reconcile.Result{}, err
	}

	if len(images) == 0 {
		nodeClass.Status.Images = nil
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, "ImagesNotFound", "ImageSelector did not match any Images")
		return reconcile.Result{}, nil
	}

	nodeClass.Status.Images = lo.Map(images, func(image imagefamily.Image, _ int) v1alpha1.Image {
		reqs := lo.Map(image.Requirements.NodeSelectorRequirements(), func(item karpv1.NodeSelectorRequirementWithMinValues, _ int) corev1.NodeSelectorRequirement {
			return item.NodeSelectorRequirement
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
