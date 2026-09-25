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

var cosVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

type ContainerOptimizedOS struct {
	computeService  *compute.Service
	versionProvider versionprovider.Provider
	cooldown        *catalogCooldown
}

func (c *ContainerOptimizedOS) ResolveImages(ctx context.Context, version string) (Images, error) {
	if version == "latest" {
		sourceImage, err := c.resolveLatestCOSImage(ctx)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to resolve COS GKE image from catalog")
			return nil, err
		}
		return c.resolveImages(sourceImage), nil
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
	sourceImage, err := c.resolveLatestCOSImage(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve COS GKE image from catalog")
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

// resolveExactBuildCOSImage selects the COS image for one GKE build without
// filtering on the server. A miss must not fall back to a different build.
func (c *ContainerOptimizedOS) resolveExactBuildCOSImage(ctx context.Context, k8sKey, build string) (string, error) {
	prefix := fmt.Sprintf("gke-%s-gke%s-", k8sKey, build)
	var best *compute.Image
	err := scanImageCatalog(ctx, c.computeService, cosImageProject, c.cooldown, func(img *compute.Image) bool {
		if strings.HasPrefix(img.Name, prefix) && isUsableCOSImage(img) {
			best = img
			return true
		}
		return false
	})
	if err != nil {
		return "", fmt.Errorf("listing COS images for GKE build gke%s: %w", build, err)
	}
	if best == nil {
		return "", &imageResolutionError{msg: fmt.Sprintf(
			"no COS image found for GKE build gke%s (k8s %s) in %s — "+
				"the channel version may not yet have a published COS image", build, k8sKey, cosImageProject)}
	}
	return fmt.Sprintf("projects/%s/global/images/%s", cosImageProject, best.Name), nil
}

// resolveLatestCOSImage selects the newest ordinary COS image for the cluster patch.
// The arm64 and GPU variants are derived from it by resolveImages.
func (c *ContainerOptimizedOS) resolveLatestCOSImage(ctx context.Context) (string, error) {
	filter := c.buildImageFilter(ctx)
	var best *compute.Image
	err := scanImageCatalog(ctx, c.computeService, cosImageProject, c.cooldown, func(img *compute.Image) bool {
		if matchesCOSNameFilter(img.Name, filter) && isUsableCOSImage(img) {
			best = img
			return true
		}
		return false
	})
	if err != nil {
		return "", fmt.Errorf("listing COS GKE images in %s: %w", cosImageProject, err)
	}
	if best == nil {
		return "", fmt.Errorf("no non-deprecated COS amd64 image found in %s (filter: %s)", cosImageProject, filter)
	}
	return fmt.Sprintf("projects/%s/global/images/%s", cosImageProject, best.Name), nil
}

func matchesCOSNameFilter(name, filter string) bool {
	if filter == `name=gke-*-cos-*-c-pre` {
		return strings.HasPrefix(name, "gke-") && strings.Contains(name, "-cos-") && strings.HasSuffix(name, "-c-pre")
	}
	return strings.HasPrefix(name, strings.TrimSuffix(strings.TrimPrefix(filter, "name="), "*"))
}

// isUsableCOSImage reports whether img is a non-deprecated general-purpose amd64 COS image
// suitable for use as a GKE node image.
func isUsableCOSImage(img *compute.Image) bool {
	if img.Status != "READY" {
		return false
	}
	if img.Deprecated != nil {
		switch img.Deprecated.State {
		case "DEPRECATED", "OBSOLETE", "DELETED":
			return false
		}
	}
	// Exclude arm64 and specialised variants. Exclude cgpv1 (cgroup v1) images:
	// GKE 1.29+ clusters run cgroup v2 by default and the GKE 1.29+ kubelet
	// sets --fail-cgroupv1=true, so cgpv1 images cause an immediate kubelet crash.
	for _, skip := range []string{"arm64", "kmod", "nvda", "gvisor", "-test", "cgpv1"} {
		if strings.Contains(img.Name, skip) {
			return false
		}
	}
	return true
}

// buildImageFilter describes the name scope to apply locally for the cluster's
// K8s patch version. It falls back to the broad GKE COS scope when unavailable.
func (c *ContainerOptimizedOS) buildImageFilter(ctx context.Context) string {
	if c.versionProvider == nil {
		return `name=gke-*-cos-*-c-pre`
	}
	k8sVer, err := c.versionProvider.Get(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get K8s version for COS image filter, using broad filter")
		return `name=gke-*-cos-*-c-pre`
	}
	parsed, err := k8sversion.ParseGeneric(k8sVer)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to parse K8s version for COS image filter, using broad filter", "version", k8sVer)
		return `name=gke-*-cos-*-c-pre`
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
