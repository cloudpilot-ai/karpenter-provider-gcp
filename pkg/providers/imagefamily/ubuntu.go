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

var ubuntuVersionRe = regexp.MustCompile(`-v\d+(-|$)`)
var ubuntuPinnedVersionRe = regexp.MustCompile(`^v\d{8}$`)

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

func (u *Ubuntu) ResolveImages(ctx context.Context, version string) (Images, error) {
	if version != "latest" && !ubuntuPinnedVersionRe.MatchString(version) {
		return nil, &imageResolutionError{msg: fmt.Sprintf(
			"invalid Ubuntu version %q: must be 'latest' or 'vYYYYMMDD' (e.g. 'v20260416')", version)}
	}

	sourceImage, err := u.resolveLatestUbuntuImage(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve Ubuntu GKE image from catalog")
		return Images{}, err
	}

	if version != "latest" {
		sourceImage = ubuntuVersionRe.ReplaceAllStringFunc(sourceImage, func(m string) string {
			suffix := ""
			if strings.HasSuffix(m, "-") {
				suffix = "-"
			}
			return "-" + version + suffix
		})
	}

	return u.resolveImages(sourceImage), nil
}

// resolveLatestUbuntuImage queries the ubuntu-os-gke-cloud project for the most recent
// non-deprecated amd64 Ubuntu GKE image matching the cluster's K8s minor version.
// The arm64 variant is derived by resolveImages.
func (u *Ubuntu) resolveLatestUbuntuImage(ctx context.Context) (string, error) {
	filter := u.buildImageFilter(ctx)

	var best *compute.Image
	err := u.computeService.Images.List(ubuntuGKEImageProject).
		Filter(filter).
		Pages(ctx, func(page *compute.ImageList) error {
			for _, img := range page.Items {
				if u.isUsable(img) && (best == nil || img.CreationTimestamp > best.CreationTimestamp) {
					best = img
				}
			}
			return nil
		})
	if err != nil {
		return "", fmt.Errorf("listing Ubuntu GKE images in %s: %w", ubuntuGKEImageProject, err)
	}
	if best == nil {
		return "", fmt.Errorf("no non-deprecated ubuntu-gke-%s amd64 image found in %s (filter: %s)",
			u.release, ubuntuGKEImageProject, filter)
	}
	return fmt.Sprintf("projects/%s/global/images/%s", ubuntuGKEImageProject, best.Name), nil
}

// isUsable dispatches to the release-specific usability check.
func (u *Ubuntu) isUsable(img *compute.Image) bool {
	if u.release == "2204" {
		return isUsableUbuntu2204Image(img)
	}
	return isUsableUbuntuImage(img)
}

// isUsableUbuntuImage reports whether img is a non-deprecated general-purpose amd64
// ubuntu-gke-2404 image. The 2404 series uses explicit "-amd64-" in the name.
func isUsableUbuntuImage(img *compute.Image) bool {
	if img.Deprecated != nil {
		switch img.Deprecated.State {
		case "DEPRECATED", "OBSOLETE", "DELETED":
			return false
		}
	}
	if !strings.Contains(img.Name, "-amd64-") {
		return false
	}
	for _, skip := range []string{"cgroupsv1", "linux64k", "-tpu-", "-test"} {
		if strings.Contains(img.Name, skip) {
			return false
		}
	}
	return true
}

// isUsableUbuntu2204Image reports whether img is a non-deprecated amd64
// ubuntu-gke-2204 image. Unlike 2404, the 2204 series does not include "-amd64-"
// in the name for amd64 images, so we exclude arm64 images instead of requiring -amd64-.
func isUsableUbuntu2204Image(img *compute.Image) bool {
	if img.Deprecated != nil {
		switch img.Deprecated.State {
		case "DEPRECATED", "OBSOLETE", "DELETED":
			return false
		}
	}
	if strings.Contains(img.Name, "-arm64-") {
		return false
	}
	for _, skip := range []string{"cgroupsv1", "linux64k", "-tpu-", "-test"} {
		if strings.Contains(img.Name, skip) {
			return false
		}
	}
	return true
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

func (u *Ubuntu) resolveImages(sourceImage string) Images {
	ret := Images{}

	// amd64
	ret = append(ret, Image{
		SourceImage: sourceImage,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement)),
	})

	// arm64: derivation is release-specific.
	arm64Image := u.deriveArm64Image(sourceImage)
	ret = append(ret, Image{
		SourceImage: arm64Image,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist)),
	})

	return ret
}

// deriveArm64Image returns the arm64 variant of a source Ubuntu image.
//   - 2404: replace "-amd64-" with "-arm64-" (explicit arch in name)
//   - 2204: insert "-arm64" before the "-v{date}" version suffix
func (u *Ubuntu) deriveArm64Image(sourceImage string) string {
	if u.release == "2404" {
		return strings.Replace(sourceImage, "-amd64-", "-arm64-", 1)
	}
	// 2204: ubuntu-gke-2204-1-34-v20231201 → ubuntu-gke-2204-1-34-arm64-v20231201
	return ubuntuVersionRe.ReplaceAllStringFunc(sourceImage, func(m string) string {
		suffix := ""
		if strings.HasSuffix(m, "-") {
			suffix = "-"
		}
		return "-arm64-v" + strings.TrimSuffix(strings.TrimPrefix(m, "-v"), "-") + suffix
	})
}
