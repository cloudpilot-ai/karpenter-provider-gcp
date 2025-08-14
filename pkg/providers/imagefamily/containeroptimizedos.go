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
	"strings"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
)

type ContainerOptimizedOS struct {
	nodePoolTemplateProvider nodepooltemplate.Provider
}

func (c *ContainerOptimizedOS) ResolveImages(ctx context.Context, nodePoolName, version string) (Images, error) {
	sourceImage, err := getSourceImage(ctx, c.nodePoolTemplateProvider, nodePoolName)
	if err != nil {
		log.FromContext(ctx).Error(err, "unable to get sourceImage")
		return nil, err
	}
	if sourceImage == "" {
		return nil, nil
	}

	if version == "latest" {
		return c.resolveImages(sourceImage), nil
	}

	// the original image is like:  projects/gke-node-images/global/images/gke-1324-gke1415000-cos-117-18613-263-14-c-pre
	// we need to replace 117-18613-263-14 to the target version
	versionRe := regexp.MustCompile(`cos-\d+-([\d-]+)-c-pre`)
	targetVersion := renderVersion(version)
	modifiedImage := versionRe.ReplaceAllString(sourceImage, "cos-"+targetVersion+"-c-pre")

	return c.resolveImages(modifiedImage), nil
}

func renderVersion(version string) string {
	targetVersion := version
	// Remove leading 'v' if present and replace '.' with '-'
	if strings.HasPrefix(version, "v") {
		targetVersion = targetVersion[1:]
	}
	return strings.ReplaceAll(targetVersion, ".", "-")
}

var (
	arm64Pattern     = `(projects\/gke-node-images\/global\/images\/gke-\d+-gke\d+-cos)-(\d+-\d+-\d+-\d+-c-pre)`
	arm64Replacement = `$1-arm64-$2`
	arm64Re          = regexp.MustCompile(arm64Pattern)
)

func (c *ContainerOptimizedOS) resolveImages(sourceImage string) Images {
	ret := Images{}

	// x86
	ret = append(ret, Image{
		SourceImage: sourceImage,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist)),
	})

	// arm64
	arm64Image := arm64Re.ReplaceAllString(sourceImage, arm64Replacement)
	ret = append(ret, Image{
		SourceImage: arm64Image,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist)),
	})

	// gpu
	gpuImages := strings.ReplaceAll(sourceImage, "-pre", "-nvda")
	ret = append(ret, Image{
		SourceImage: gpuImages,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpExists)),
	})

	return ret
}
