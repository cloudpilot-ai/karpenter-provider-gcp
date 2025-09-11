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
	"sync"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"google.golang.org/api/compute/v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

type Provider interface {
	List(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (Images, error)
}

type DefaultProvider struct {
	sync.Mutex
	cache          *cache.Cache
	computeService *compute.Service

	containerOptimizedOSProvider *ContainerOptimizedOS
	ubuntuOSProvider             *Ubuntu
}

func NewDefaultProvider(computeService *compute.Service, nodePoolTemplateProvider nodepooltemplate.Provider) *DefaultProvider {
	return &DefaultProvider{
		cache:          cache.New(pkgcache.ImageCacheExpirationPeriod, pkgcache.DefaultCleanupInterval),
		computeService: computeService,
		containerOptimizedOSProvider: &ContainerOptimizedOS{
			nodePoolTemplateProvider: nodePoolTemplateProvider,
		},
		ubuntuOSProvider: &Ubuntu{
			nodePoolTemplateProvider: nodePoolTemplateProvider,
		},
	}
}

func (p *DefaultProvider) List(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (Images, error) {
	p.Lock()
	defer p.Unlock()

	hash, err := hashstructure.Hash(nodeClass.Spec.ImageSelectorTerms, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	if err != nil {
		return nil, err
	}
	if images, ok := p.cache.Get(fmt.Sprintf("%d", hash)); ok {
		return images.(Images), nil
	}

	images, ok, err := p.resolveImageFromAlias(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	if ok {
		p.cache.SetDefault(fmt.Sprintf("%d", hash), images)
		return images, nil
	}

	images, _, err = p.resolveImageFromID(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	p.cache.SetDefault(fmt.Sprintf("%d", hash), images)
	return images, nil
}

func (p *DefaultProvider) resolveImageFromID(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (Images, bool, error) {
	images := Images{}
	for _, term := range nodeClass.Spec.ImageSelectorTerms {
		image, err := p.resolveImage(term.ID)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to resolve image", "imageSource", term.ID)
			return nil, false, err
		}
		if image == nil {
			continue
		}

		var requirements scheduling.Requirements
		switch image.Architecture {
		case OSArchitectureX86:
			requirements = scheduling.NewRequirements(
				scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement),
			)
		case OSArchitectureARM:
			requirements = scheduling.NewRequirements(
				scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
			)
		default:
			log.FromContext(ctx).Error(err, "unsupported architecture", "imageSource", term.ID)
			return nil, false, fmt.Errorf("unsupported architecture: %s", image.Architecture)
		}

		images = append(images, Image{
			SourceImage:  term.ID,
			Requirements: requirements,
		})
	}

	return images, true, nil
}

func (p *DefaultProvider) resolveImageFromAlias(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (Images, bool, error) {
	alias := nodeClass.Alias()
	if alias == nil {
		return Images{}, false, nil
	}

	images := Images{}
	familyProvider := p.getImageFamilyProvider(alias.Family)
	ims, err := familyProvider.ResolveImages(ctx, utils.ResolveNodePoolName(nodeClass.Name), alias.Version)
	if err != nil {
		return nil, false, err
	}

	// Ensure the image exists in GCP
	for _, im := range ims {
		gceim, err := p.resolveImage(im.SourceImage)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to resolve image", "imageSource", im.SourceImage)
			return nil, false, err
		}
		if gceim == nil {
			continue
		}

		images = append(images, im)
	}

	return images, true, nil
}

func (p *DefaultProvider) resolveImage(sourceImage string) (*compute.Image, error) {
	projectID, imageName, err := parseImageSource(sourceImage)
	if err != nil {
		return nil, err
	}

	image, err := p.computeService.Images.Get(projectID, imageName).Do()
	if err != nil && !strings.Contains(err.Error(), "404") {
		return nil, err
	}

	return image, nil
}

// parseImageSource parses a GCP image source string
// Supports formats like:
// - projects/PROJECT_ID/global/images/IMAGE_NAME
// - global/images/IMAGE_NAME (requires project context)
// - IMAGE_NAME (requires project context)
func parseImageSource(imageSource string) (projectID, imageName string, err error) {
	// Remove any leading/trailing whitespace
	imageSource = strings.TrimSpace(imageSource)

	// Pattern: projects/PROJECT_ID/global/images/IMAGE_NAME
	projectPattern := regexp.MustCompile(`^projects/([^/]+)/global/images/(.+)$`)
	if matches := projectPattern.FindStringSubmatch(imageSource); matches != nil {
		return matches[1], matches[2], nil
	}

	// Pattern: global/images/IMAGE_NAME
	globalPattern := regexp.MustCompile(`^global/images/(.+)$`)
	if matches := globalPattern.FindStringSubmatch(imageSource); matches != nil {
		return "", "", fmt.Errorf("project ID required for global image reference: %s", imageSource)
	}

	// Pattern: IMAGE_NAME only
	if !strings.Contains(imageSource, "/") {
		return "", "", fmt.Errorf("project ID required for image name: %s", imageSource)
	}

	return "", "", fmt.Errorf("invalid image source format: %s", imageSource)
}

func (p *DefaultProvider) getImageFamilyProvider(family string) ImageFamily {
	switch family {
	case v1alpha1.ImageFamilyUbuntu:
		return p.ubuntuOSProvider
	case v1alpha1.ImageFamilyContainerOptimizedOS:
		return p.containerOptimizedOSProvider
	default:
		return nil
	}
}
