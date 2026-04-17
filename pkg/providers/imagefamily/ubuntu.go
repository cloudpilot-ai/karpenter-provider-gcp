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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

const ubuntuGKEImageProject = "ubuntu-os-gke-cloud"

type Ubuntu struct {
	computeService *compute.Service
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
// non-deprecated Ubuntu GKE image for amd64. The arm64 variant is derived from it
// by resolveImages using the existing regex replacement.
func (u *Ubuntu) resolveLatestUbuntuImage(ctx context.Context) (string, error) {
	// List images in ubuntu-os-gke-cloud, filter to ubuntu-gke prefix (amd64), most recent first.
	resp, err := u.computeService.Images.List(ubuntuGKEImageProject).
		Filter(`name:"ubuntu-gke-2204"`).
		OrderBy("creationTimestamp desc").
		MaxResults(50).
		Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("listing Ubuntu GKE images in %s: %w", ubuntuGKEImageProject, err)
	}

	for _, img := range resp.Items {
		// Skip deprecated images.
		if img.Deprecated != nil && img.Deprecated.State != "" {
			continue
		}
		// Skip arm64 images; amd64 is the source from which arm64 is derived.
		if strings.Contains(img.Name, "arm64") {
			continue
		}
		return fmt.Sprintf("projects/%s/global/images/%s", ubuntuGKEImageProject, img.Name), nil
	}

	return "", fmt.Errorf("no non-deprecated ubuntu-gke-2204 image found in %s", ubuntuGKEImageProject)
}

var (
	ubuntuArm64Pattern     = `(projects\/ubuntu-os-gke-cloud\/global\/images\/ubuntu-gke-.*?)(?:-amd64)?-v(\d+)`
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
