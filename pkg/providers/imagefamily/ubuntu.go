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

package imagefamily

import (
	"context"
	"regexp"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
)

type Ubuntu struct {
	nodePoolTemplateProvider nodepooltemplate.Provider
}

func (u *Ubuntu) ResolveImages(ctx context.Context, nodePoolName, version string) (Images, error) {
	sourceImage, err := getSourceImage(ctx, u.nodePoolTemplateProvider, nodePoolName)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to get source image")
		return Images{}, err
	}
	if sourceImage == "" {
		return nil, nil
	}

	if version != "latest" {
		re := regexp.MustCompile(`v\d+$`)
		sourceImage = re.ReplaceAllString(sourceImage, version)
	}
	return u.resolveImages(sourceImage), nil
}

var (
	ubuntuArm64Pattern     = `(projects\/ubuntu-os-gke-cloud\/global\/images\/ubuntu-gke-\d+-\d+-\d+)-v(\d+)`
	ubuntuArm64Replacement = `$1-arm64-$2`
	ubuntuArm64Re          = regexp.MustCompile(ubuntuArm64Pattern)
)

func (u *Ubuntu) resolveImages(sourceImage string) Images {
	ret := Images{}

	// x86 & gpu
	ret = append(ret, Image{
		SourceImage: sourceImage,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement)),
	})

	// arm64
	arm64Image := ubuntuArm64Re.ReplaceAllString(sourceImage, ubuntuArm64Replacement)
	ret = append(ret, Image{
		SourceImage: arm64Image,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist)),
	})

	return ret
}
