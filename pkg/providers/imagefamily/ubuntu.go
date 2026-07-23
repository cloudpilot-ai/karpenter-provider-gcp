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

const ubuntuGKEImageProject = "ubuntu-os-gke-cloud"

// ubuntuPinnedVersionRe validates a pinned image version (vYYYYMMDD).
var ubuntuPinnedVersionRe = regexp.MustCompile(`^v\d{8}$`)

// Clean-name allowlists per release and architecture: anchoring on the version end rejects
// variant builds (-tpu, -cgroupsv1, -linux64k, ...) while allowing a trailing date letter
// (v20251218a). 2204 amd64 names carry no arch token.
var (
	ubuntu2404AMD64Re = regexp.MustCompile(`^ubuntu-gke-2404-\d+-\d+-amd64-v\d+[a-z]?$`)
	ubuntu2404ARM64Re = regexp.MustCompile(`^ubuntu-gke-2404-\d+-\d+-arm64-v\d+[a-z]?$`)
	ubuntu2204AMD64Re = regexp.MustCompile(`^ubuntu-gke-2204-\d+-\d+-v\d+[a-z]?$`)
	ubuntu2204ARM64Re = regexp.MustCompile(`^ubuntu-gke-2204-\d+-\d+-arm64-v\d+[a-z]?$`)
)

// Ubuntu resolves GKE Ubuntu images from the ubuntu-os-gke-cloud project.
// The release field selects the OS series: "2404" for Ubuntu 24.04 or "2204" for Ubuntu 22.04.
type Ubuntu struct {
	computeService  *compute.Service
	versionProvider versionprovider.Provider
	release         string // "2404" or "2204"
}

func (u *Ubuntu) imagePrefix() string {
	return "ubuntu-gke-" + u.release
}

// architecture requirements this provider resolves an image for, in output order.
var ubuntuArchitectures = []string{OSArchAMD64Requirement, OSArchARM64Requirement}

func (u *Ubuntu) ResolveImages(ctx context.Context, version string) (Images, error) {
	if version != "latest" && !ubuntuPinnedVersionRe.MatchString(version) {
		return nil, &imageResolutionError{msg: fmt.Sprintf(
			"invalid Ubuntu version %q: must be 'latest' or 'vYYYYMMDD' (e.g. 'v20260416')", version)}
	}

	images, err := u.listImages(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve Ubuntu GKE image from catalog")
		return Images{}, err
	}

	// Resolve each arch to a real listed image; a missing one (incl. a pinned date absent for
	// one arch) fails resolution instead of a single-arch set that reads as ImagesReady=True.
	ret := Images{}
	for _, arch := range ubuntuArchitectures {
		name, ok := u.selectImage(images, arch, version)
		if !ok {
			return nil, &imageResolutionError{msg: fmt.Sprintf(
				"no ubuntu-gke-%s %s image found for version %q in %s",
				u.release, arch, version, ubuntuGKEImageProject)}
		}
		ret = append(ret, Image{
			SourceImage:  fmt.Sprintf("projects/%s/global/images/%s", ubuntuGKEImageProject, name),
			Requirements: ubuntuRequirementsForArch(arch),
		})
	}
	return ret, nil
}

// listImages returns all Ubuntu GKE images matching the cluster's K8s minor version.
func (u *Ubuntu) listImages(ctx context.Context) ([]*compute.Image, error) {
	filter := u.buildImageFilter(ctx)
	var images []*compute.Image
	err := u.computeService.Images.List(ubuntuGKEImageProject).
		Filter(filter).
		Pages(ctx, func(page *compute.ImageList) error {
			images = append(images, page.Items...)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("listing Ubuntu GKE images in %s: %w", ubuntuGKEImageProject, err)
	}
	return images, nil
}

// selectImage returns the newest usable image name for arch ("latest") or the exact-dated
// usable image for a pinned vYYYYMMDD; ok=false if none matches, so a pinned date missing for
// one arch fails resolution rather than resolving a single-arch set.
func (u *Ubuntu) selectImage(images []*compute.Image, arch, version string) (string, bool) {
	var best *compute.Image
	for _, img := range images {
		if !u.isUsableForArch(img, arch) {
			continue
		}
		if version != "latest" && !strings.HasSuffix(img.Name, "-"+version) {
			continue
		}
		if best == nil || img.CreationTimestamp > best.CreationTimestamp {
			best = img
		}
	}
	if best == nil {
		return "", false
	}
	return best.Name, true
}

// isUsableForArch dispatches to the release- and architecture-specific usability check.
func (u *Ubuntu) isUsableForArch(img *compute.Image, arch string) bool {
	switch {
	case u.release == "2204" && arch == OSArchARM64Requirement:
		return isUsableUbuntu2204Arm64Image(img)
	case u.release == "2204":
		return isUsableUbuntu2204Image(img)
	case arch == OSArchARM64Requirement:
		return isUsableUbuntuArm64Image(img)
	default:
		return isUsableUbuntuImage(img)
	}
}

// ubuntuRequirementsForArch builds the scheduling requirements for a resolved image.
func ubuntuRequirementsForArch(arch string) scheduling.Requirements {
	if arch == OSArchARM64Requirement {
		return scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist))
	}
	return scheduling.NewRequirements(
		scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement))
}

// ubuntuImageDeprecated reports whether img has been retired by GCP (an empty state is active).
func ubuntuImageDeprecated(img *compute.Image) bool {
	if img.Deprecated == nil {
		return false
	}
	switch img.Deprecated.State {
	case "DEPRECATED", "OBSOLETE", "DELETED":
		return true
	}
	return false
}

// isUsableUbuntuImage reports whether img is a non-deprecated, clean amd64 ubuntu-gke-2404 image.
func isUsableUbuntuImage(img *compute.Image) bool {
	return !ubuntuImageDeprecated(img) && ubuntu2404AMD64Re.MatchString(img.Name)
}

// isUsableUbuntuArm64Image reports whether img is a non-deprecated, clean arm64 ubuntu-gke-2404 image.
func isUsableUbuntuArm64Image(img *compute.Image) bool {
	return !ubuntuImageDeprecated(img) && ubuntu2404ARM64Re.MatchString(img.Name)
}

// isUsableUbuntu2204Image reports whether img is a non-deprecated, clean amd64 ubuntu-gke-2204 image
// (the 2204 series carries no arch token for amd64).
func isUsableUbuntu2204Image(img *compute.Image) bool {
	return !ubuntuImageDeprecated(img) && ubuntu2204AMD64Re.MatchString(img.Name)
}

// isUsableUbuntu2204Arm64Image reports whether img is a non-deprecated, clean arm64 ubuntu-gke-2204 image.
func isUsableUbuntu2204Arm64Image(img *compute.Image) bool {
	return !ubuntuImageDeprecated(img) && ubuntu2204ARM64Re.MatchString(img.Name)
}

// buildImageFilter returns a GCP Images.List filter scoped to the cluster's K8s minor version.
func (u *Ubuntu) buildImageFilter(ctx context.Context) string {
	prefix := u.imagePrefix()
	if u.versionProvider == nil {
		return fmt.Sprintf(`name=%s*`, prefix)
	}
	k8sVer, err := u.versionProvider.Get(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get K8s version for Ubuntu image filter, using broad filter")
		return fmt.Sprintf(`name=%s*`, prefix)
	}
	parsed, err := k8sversion.ParseGeneric(k8sVer)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to parse K8s version for Ubuntu image filter, using broad filter", "version", k8sVer)
		return fmt.Sprintf(`name=%s*`, prefix)
	}
	return fmt.Sprintf(`name=%s-%d-%d*`, prefix, parsed.Major(), parsed.Minor())
}
