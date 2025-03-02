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
	"errors"
	"fmt"
	"regexp"
	"strings"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/alibabacloud-go/tea/tea"
	"google.golang.org/api/iterator"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

type ContainerOptimizedOS struct {
	imageClient *compute.ImagesClient
}

func resolveImagePrefix(serverVersion string) (string, error) {
	re := regexp.MustCompile(`v1\.(\d+)\.(\d+)-gke\.(\d+)`)
	matches := re.FindStringSubmatch(serverVersion)

	if len(matches) < 4 {
		return "", fmt.Errorf("invalid server version format: %s", serverVersion)
	}

	// v1.30.8-gke.1009000
	major := matches[1] // "30"
	minor := matches[2] // "9"
	build := matches[3] // "1009000"

	// Format as expected GKE image name prefix
	imagePrefix := fmt.Sprintf("gke-1%s%s-gke%s-", major, minor, build)
	return imagePrefix, nil
}

func resolveSourceImage(selfLink string) (string, error) {
	// format: https://www.googleapis.com/compute/v1/projects/gke-node-images/global/images/gke-1309-gke1046000-cos-arm64-113-18244-291-9-c-pre
	re := regexp.MustCompile(`compute/v1\/(.*)`)
	matches := re.FindStringSubmatch(selfLink)
	if len(matches) < 2 {
		return "", fmt.Errorf("invalid selfLink format: %s", selfLink)
	}
	return matches[1], nil
}

func resolveImageRequirements(image *computepb.Image) scheduling.Requirements {
	ret := scheduling.NewRequirements()
	if tea.StringValue(image.Architecture) == "ARM64" {
		current := scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, "arm64")
		ret.Add(current)
	}
	if tea.StringValue(image.Architecture) == "X86_64" {
		current := scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, "amd64")
		ret.Add(current)
	}

	imgName := tea.StringValue(image.Name)
	if strings.HasSuffix(imgName, "-nvda") {
		current := scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpExists)
		ret.Add(current)
	} else {
		current := scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, v1.NodeSelectorOpDoesNotExist)
		ret.Add(current)
	}

	return ret
}

const (
	ContainerOptimizedOSProject = "gke-node-images"
)

func (c *ContainerOptimizedOS) ResolveImages(ctx context.Context, serverVersion string) (Images, error) {
	prefix, err := resolveImagePrefix(serverVersion)
	if err != nil {
		log.FromContext(ctx).Error(err, "error getting image prefix")
		return nil, err
	}
	filter := fmt.Sprintf("(name eq %s.*)", prefix)

	req := &computepb.ListImagesRequest{
		Project: ContainerOptimizedOSProject,
		Filter:  &filter,
	}

	it := c.imageClient.List(ctx, req)
	validImageRegex := regexp.MustCompile(`.*-c-(pre|nvda)$`)
	var ret Images
	for {
		image, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			log.FromContext(ctx).Error(err, "listing images error")
			return nil, err
		}

		// TODO: not find a better way to filter the images
		if !validImageRegex.MatchString(*image.Name) {
			continue
		}

		sourceImage, err := resolveSourceImage(tea.StringValue(image.SelfLink))
		if err != nil {
			log.FromContext(ctx).Error(err, "error getting image ID")
			return nil, err
		}
		ret = append(ret, Image{
			SourceImage:  sourceImage,
			Requirements: resolveImageRequirements(image),
		})
	}
	return ret, nil
}
