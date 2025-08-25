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

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
)

func resolveSourceImage(selfLink string) (string, error) {
	// format: https://www.googleapis.com/compute/v1/projects/gke-node-images/global/images/gke-1309-gke1046000-cos-arm64-113-18244-291-9-c-pre
	re := regexp.MustCompile(`compute/v1\/(.*)`)
	matches := re.FindStringSubmatch(selfLink)
	if len(matches) < 2 {
		return "", fmt.Errorf("invalid selfLink format: %s", selfLink)
	}
	return matches[1], nil
}

func getSourceImage(ctx context.Context, provider nodepooltemplate.Provider, nodePoolName string) (string, error) {
	defaultNodeTemplate, err := provider.GetInstanceTemplate(ctx, nodePoolName)
	if err != nil {
		return "", fmt.Errorf("failed to get default node template: %w", err)
	}
	if defaultNodeTemplate == nil {
		return "", nil
	}

	systemDisk, ok := lo.Find(defaultNodeTemplate.Properties.Disks, func(item *compute.AttachedDisk) bool {
		return item.Boot
	})
	if !ok {
		return "", fmt.Errorf("failed to find boot disk")
	}

	imageSelfLink := systemDisk.InitializeParams.SourceImage
	sourceImage, err := resolveSourceImage(imageSelfLink)
	if err != nil {
		return "", err
	}

	return sourceImage, nil
}
