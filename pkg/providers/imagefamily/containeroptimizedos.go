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

// Drop only the patch scope when the cluster version is unavailable.
const broadCOSImageFilter = `name=gke-*`

// GKE publishes COS variants independently.
type cosVariant int

const (
	cosVariantAMD64CPU cosVariant = iota
	cosVariantARM64CPU
	cosVariantAMD64NVIDIA
)

// cosVariantOrder is the stable order of published Images.
var cosVariantOrder = []cosVariant{cosVariantAMD64CPU, cosVariantARM64CPU, cosVariantAMD64NVIDIA}

// Anchored names exclude specialized builds, including cgroup-v1 images incompatible with GKE 1.29+.
var (
	cosAMD64CPUNameRe    = regexp.MustCompile(`^gke-\d+-gke\d+-cos-\d+-\d+-\d+-\d+-c-pre$`)
	cosARM64CPUNameRe    = regexp.MustCompile(`^gke-\d+-gke\d+-cos-arm64-\d+-\d+-\d+-\d+-c-pre$`)
	cosAMD64NVIDIANameRe = regexp.MustCompile(`^gke-\d+-gke\d+-cos-\d+-\d+-\d+-\d+-c-nvda$`)
)

var cosVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

type ContainerOptimizedOS struct {
	computeService  *compute.Service
	versionProvider versionprovider.Provider
}

func (c *ContainerOptimizedOS) ResolveImages(ctx context.Context, version string) (Images, error) {
	if version == "latest" {
		images, err := c.resolveLatestCOSImages(ctx)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to resolve COS GKE images from image project")
			return nil, err
		}
		return images, nil
	}

	if k8sKey, build, ok := ParseGKEVersion(version); ok {
		// Channel-based resolution: use exact-build filter; no fallback on miss.
		sourceImage, err := c.resolveExactBuildCOSImage(ctx, k8sKey, build)
		if err != nil {
			return nil, err
		}
		return c.resolveImages(sourceImage), nil
	}

	// Explicit milestone version pin via alias (e.g. "125.19216.104.126").
	if !cosVersionRe.MatchString(version) {
		return nil, &imageResolutionError{msg: fmt.Sprintf(
			"invalid ContainerOptimizedOS version %q: must be 'latest', a GKE version (e.g. '1.34.6-gke.1068000'), or 'milestone.build.build.build' (e.g. '125.19216.104.126')", version)}
	}
	sourceImage, err := c.resolveMilestoneBaseCOSImage(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve COS GKE image from image project")
		return nil, err
	}
	versionRe := regexp.MustCompile(`cos-\d+-([\d-]+)-c-pre`)
	targetVersion := renderVersion(version)
	modifiedImage := versionRe.ReplaceAllString(sourceImage, "cos-"+targetVersion+"-c-pre")
	return c.resolveImages(modifiedImage), nil
}

// ParseGKEVersion parses a GKE version string (e.g. "1.34.6-gke.1068000") into
// the image name key ("1346") and build number ("1068000"). Returns ok=false if
// v is not a GKE version string.
func ParseGKEVersion(v string) (k8sKey, build string, ok bool) {
	parts := strings.SplitN(v, "-gke.", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	base := strings.TrimPrefix(parts[0], "v")
	return strings.ReplaceAll(base, ".", ""), parts[1], true
}

// Exact builds never fall back to another build when publication is incomplete.
func (c *ContainerOptimizedOS) resolveExactBuildCOSImage(ctx context.Context, k8sKey, build string) (string, error) {
	prefix := fmt.Sprintf("gke-%s-gke%s-", k8sKey, build)
	var best *compute.Image
	err := scanImages(ctx, c.computeService, cosImageProject, func(img *compute.Image) bool {
		variant, ok := usableCOSVariant(img, prefix)
		if ok && variant == cosVariantAMD64CPU {
			best = img
			return true
		}
		return false
	})
	if err != nil {
		return "", fmt.Errorf("listing COS images for GKE build gke%s: %w", build, err)
	}
	if best == nil {
		return "", &imageUnavailableError{msg: fmt.Sprintf(
			"no usable COS image is published for GKE build gke%s (k8s %s) in %s yet — "+
				"the channel version may not have a published COS image", build, k8sKey, cosImageProject)}
	}
	return fmt.Sprintf("projects/%s/global/images/%s", cosImageProject, best.Name), nil
}

// Select the newest validated image per variant, continuing to older candidates during publication.
func (c *ContainerOptimizedOS) resolveLatestCOSImages(ctx context.Context) (Images, error) {
	filter := c.buildImageFilter(ctx)
	scope := cosImageNameScope(filter)
	selected := make(map[cosVariant]*compute.Image, len(cosVariantOrder))
	var lookupErr error
	err := scanImages(ctx, c.computeService, cosImageProject, func(img *compute.Image) bool {
		variant, ok := usableCOSVariant(img, scope)
		if !ok || selected[variant] != nil {
			return false
		}
		candidate, err := lookupImage(ctx, c.computeService, cosImageProject, img.Name)
		if err != nil {
			lookupErr = fmt.Errorf("validating COS image %s: %w", img.Name, err)
			return true
		}
		// The listing may be stale; do not let a vanished or unusable candidate displace an older one.
		if candidate != nil && imageUsable(candidate) {
			selected[variant] = candidate
		}
		return len(selected) == len(cosVariantOrder)
	})
	if err != nil {
		return nil, fmt.Errorf("listing COS GKE images in %s: %w", cosImageProject, err)
	}
	if lookupErr != nil {
		return nil, lookupErr
	}
	if len(selected) == 0 {
		return nil, &imageUnavailableError{msg: fmt.Sprintf(
			"no usable ContainerOptimizedOS image is published in %s for filter %s: "+
				"no READY, non-deprecated COS image matches the selected build yet", cosImageProject, filter)}
	}
	images := make(Images, 0, len(selected))
	for _, variant := range cosVariantOrder {
		if img := selected[variant]; img != nil {
			images = append(images, Image{
				SourceImage:  fmt.Sprintf("projects/%s/global/images/%s", cosImageProject, img.Name),
				Requirements: cosVariantRequirements(variant),
			})
		}
	}
	return images, nil
}

func usableCOSVariant(img *compute.Image, scope string) (cosVariant, bool) {
	variant, ok := classifyCOSImage(img.Name)
	return variant, ok && strings.HasPrefix(img.Name, scope) && imageUsable(img)
}

// classifyCOSImage maps a COS image name to its supported publication variant.
func classifyCOSImage(name string) (cosVariant, bool) {
	switch {
	case cosAMD64CPUNameRe.MatchString(name):
		return cosVariantAMD64CPU, true
	case cosARM64CPUNameRe.MatchString(name):
		return cosVariantARM64CPU, true
	case cosAMD64NVIDIANameRe.MatchString(name):
		return cosVariantAMD64NVIDIA, true
	default:
		return 0, false
	}
}

// cosVariantRequirements returns the scheduling requirements of one COS variant.
func cosVariantRequirements(variant cosVariant) scheduling.Requirements {
	switch variant {
	case cosVariantARM64CPU:
		return scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist))
	case cosVariantAMD64NVIDIA:
		return scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpExists))
	default:
		return scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist))
	}
}

// Milestone pins retain the existing name-template construction from an amd64 CPU base.
func (c *ContainerOptimizedOS) resolveMilestoneBaseCOSImage(ctx context.Context) (string, error) {
	filter := c.buildImageFilter(ctx)
	scope := cosImageNameScope(filter)
	var best *compute.Image
	err := scanImages(ctx, c.computeService, cosImageProject, func(img *compute.Image) bool {
		variant, ok := usableCOSVariant(img, scope)
		if ok && variant == cosVariantAMD64CPU {
			best = img
			return true
		}
		return false
	})
	if err != nil {
		return "", fmt.Errorf("listing COS GKE images in %s: %w", cosImageProject, err)
	}
	if best == nil {
		return "", &imageUnavailableError{msg: fmt.Sprintf("no usable COS amd64 image is published in %s for filter %s yet, so a milestone pin cannot be resolved", cosImageProject, filter)}
	}
	return fmt.Sprintf("projects/%s/global/images/%s", cosImageProject, best.Name), nil
}

// cosImageNameScope returns the image-name prefix bounded by buildImageFilter. The scope only
// limits the GKE build; variant classification is applied separately.
func cosImageNameScope(filter string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filter, "name="), "*")
}

// buildImageFilter describes the name scope to apply locally for the cluster's
// K8s patch version. It falls back to the broad GKE COS scope when unavailable.
func (c *ContainerOptimizedOS) buildImageFilter(ctx context.Context) string {
	if c.versionProvider == nil {
		return broadCOSImageFilter
	}
	k8sVer, err := c.versionProvider.Get(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get K8s version for COS image filter, using broad filter")
		return broadCOSImageFilter
	}
	parsed, err := k8sversion.ParseGeneric(k8sVer)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to parse K8s version for COS image filter, using broad filter", "version", k8sVer)
		return broadCOSImageFilter
	}
	return fmt.Sprintf(`name=gke-%d%d%d-*`, parsed.Major(), parsed.Minor(), parsed.Patch())
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
	// arm64
	arm64Image := arm64Re.ReplaceAllString(sourceImage, arm64Replacement)
	// gpu
	nvidiaImage := strings.ReplaceAll(sourceImage, "-pre", "-nvda")

	return Images{
		{SourceImage: sourceImage, Requirements: cosVariantRequirements(cosVariantAMD64CPU)},
		{SourceImage: arm64Image, Requirements: cosVariantRequirements(cosVariantARM64CPU)},
		{SourceImage: nvidiaImage, Requirements: cosVariantRequirements(cosVariantAMD64NVIDIA)},
	}
}
