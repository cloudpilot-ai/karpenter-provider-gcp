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

const ubuntuGKEImageProject = "ubuntu-os-gke-cloud"

type Ubuntu struct {
	computeService  *compute.Service
	versionProvider versionprovider.Provider
}

func (u *Ubuntu) ResolveImages(ctx context.Context, version string) (Images, error) {
	sourceImage, err := u.resolveLatestUbuntuImage(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve Ubuntu GKE image from catalog")
		return Images{}, err
	}

	if version != "latest" {
		re := regexp.MustCompile(`-v\d+(-|$)`)
		sourceImage = re.ReplaceAllStringFunc(sourceImage, func(m string) string {
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
// non-deprecated Ubuntu 24.04 GKE image for amd64 that matches the cluster's K8s minor
// version. The arm64 variant is derived from it by resolveImages via a simple string
// replacement.
//
// ubuntu-gke-2404 images use explicit arch in the name (e.g. ubuntu-gke-2404-1-35-amd64-v20260416),
// unlike the older ubuntu-gke-2204 series which had no arch suffix on amd64 images.
func (u *Ubuntu) resolveLatestUbuntuImage(ctx context.Context) (string, error) {
	filter := u.buildImageFilter(ctx)

	// GCP does not support Filter + OrderBy together, and there can be thousands of
	// ubuntu-gke-2404 images (most deprecated). We page through all of them,
	// collect non-deprecated amd64 candidates, then sort in code.
	var candidates []*compute.Image
	err := u.computeService.Images.List(ubuntuGKEImageProject).
		Filter(filter).
		Pages(ctx, func(page *compute.ImageList) error {
			for _, img := range page.Items {
				if isUsableUbuntuImage(img) {
					candidates = append(candidates, img)
				}
			}
			return nil
		})
	if err != nil {
		return "", fmt.Errorf("listing Ubuntu GKE images in %s: %w", ubuntuGKEImageProject, err)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no non-deprecated ubuntu-gke-2404 amd64 image found in %s (filter: %s)", ubuntuGKEImageProject, filter)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreationTimestamp > candidates[j].CreationTimestamp
	})
	img := candidates[0]
	return fmt.Sprintf("projects/%s/global/images/%s", ubuntuGKEImageProject, img.Name), nil
}

// isUsableUbuntuImage reports whether img is a non-deprecated general-purpose amd64
// ubuntu-gke-2404 image suitable for use as a GKE node image.
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

// buildImageFilter returns a GCP Images.List filter string scoped to the cluster's
// K8s minor version (e.g. "name=ubuntu-gke-2404-1-35*").
// Falls back to "ubuntu-gke-2404*" if the version provider is unavailable.
func (u *Ubuntu) buildImageFilter(ctx context.Context) string {
	if u.versionProvider == nil {
		return `name=ubuntu-gke-2404*`
	}
	k8sVer, err := u.versionProvider.Get(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get K8s version for Ubuntu image filter, using broad filter")
		return `name=ubuntu-gke-2404*`
	}
	parsed, err := k8sversion.ParseGeneric(k8sVer)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to parse K8s version for Ubuntu image filter, using broad filter", "version", k8sVer)
		return `name=ubuntu-gke-2404*`
	}
	return fmt.Sprintf(`name=ubuntu-gke-2404-%d-%d*`, parsed.Major(), parsed.Minor())
}

func (u *Ubuntu) resolveImages(sourceImage string) Images {
	ret := Images{}

	// x86 & gpu
	ret = append(ret, Image{
		SourceImage: sourceImage,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement)),
	})

	// arm64: ubuntu-gke-2404 uses explicit "-amd64-" in the name; replace it with "-arm64-".
	arm64Image := strings.Replace(sourceImage, "-amd64-", "-arm64-", 1)
	ret = append(ret, Image{
		SourceImage: arm64Image,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist)),
	})

	return ret
}
