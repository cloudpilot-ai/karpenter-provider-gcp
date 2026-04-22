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
	"fmt"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/api/compute/v1"
	v1 "k8s.io/api/core/v1"
	k8sversion "k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	versionprovider "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

// cosImageProject is the GCP project that owns all Container-Optimized OS GKE images.
const cosImageProject = "gke-node-images"

type ContainerOptimizedOS struct {
	computeService  *compute.Service
	versionProvider versionprovider.Provider
}

func (c *ContainerOptimizedOS) ResolveImages(ctx context.Context, version string) (Images, error) {
	sourceImage, err := c.resolveLatestCOSImage(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve COS GKE image from catalog")
		return nil, err
	}

	if version == "latest" {
		return c.resolveImages(sourceImage), nil
	}

	// Replace the version segment in the image name with the requested version.
	// Image format: gke-{major}{minor}{patch}-gke{kernel}-cos-{milestone}-...-c-pre
	versionRe := regexp.MustCompile(`cos-\d+-([\d-]+)-c-pre`)
	targetVersion := renderVersion(version)
	modifiedImage := versionRe.ReplaceAllString(sourceImage, "cos-"+targetVersion+"-c-pre")

	return c.resolveImages(modifiedImage), nil
}

// resolveLatestCOSImage queries the gke-node-images project for the most recent
// non-deprecated amd64 COS GKE image matching the cluster's K8s patch version.
// The arm64 and GPU variants are derived from it by resolveImages.
func (c *ContainerOptimizedOS) resolveLatestCOSImage(ctx context.Context) (string, error) {
	filter := c.buildImageFilter(ctx)

	var candidates []*compute.Image
	err := c.computeService.Images.List(cosImageProject).
		Filter(filter).
		Pages(ctx, func(page *compute.ImageList) error {
			for _, img := range page.Items {
				if isUsableCOSImage(img) {
					candidates = append(candidates, img)
				}
			}
			return nil
		})
	if err != nil {
		return "", fmt.Errorf("listing COS GKE images in %s: %w", cosImageProject, err)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no non-deprecated COS amd64 image found in %s (filter: %s)", cosImageProject, filter)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreationTimestamp > candidates[j].CreationTimestamp
	})
	img := candidates[0]
	return fmt.Sprintf("projects/%s/global/images/%s", cosImageProject, img.Name), nil
}

// isUsableCOSImage reports whether img is a non-deprecated general-purpose amd64 COS image
// suitable for use as a GKE node image.
func isUsableCOSImage(img *compute.Image) bool {
	if img.Deprecated != nil {
		switch img.Deprecated.State {
		case "DEPRECATED", "OBSOLETE", "DELETED":
			return false
		}
	}
	// Exclude arm64 and specialised variants.
	for _, skip := range []string{"arm64", "kmod", "nvda", "gvisor", "-test"} {
		if strings.Contains(img.Name, skip) {
			return false
		}
	}
	return true
}

// buildImageFilter returns a GCP Images.List filter string scoped to the cluster's
// K8s patch version (e.g. "name:gke-1351-*"). Falls back to "name:gke-*-cos-*-c-pre"
// if the version provider is unavailable.
func (c *ContainerOptimizedOS) buildImageFilter(ctx context.Context) string {
	if c.versionProvider == nil {
		return `name:gke-*-cos-*-c-pre`
	}
	k8sVer, err := c.versionProvider.Get(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get K8s version for COS image filter, using broad filter")
		return `name:gke-*-cos-*-c-pre`
	}
	parsed, err := k8sversion.ParseGeneric(k8sVer)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to parse K8s version for COS image filter, using broad filter", "version", k8sVer)
		return `name:gke-*-cos-*-c-pre`
	}
	return fmt.Sprintf(`name:gke-%d%d%d-*`, parsed.Major(), parsed.Minor(), parsed.Patch())
}

func renderVersion(version string) string {
	targetVersion := version
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
