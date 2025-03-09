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
	"sync"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

type Provider interface {
	List(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (Images, error)
}

type DefaultProvider struct {
	sync.Mutex
	cache *cache.Cache

	versionProvider              version.Provider
	containerOptimizedOSProvider *ContainerOptimizedOS
	ubuntuOSProvider             *Ubuntu
}

func NewDefaultProvider(versionProvider version.Provider, nodePoolTemplateProvider nodepooltemplate.Provider) *DefaultProvider {
	return &DefaultProvider{
		cache:           cache.New(pkgcache.ImageCacheExpirationPeriod, pkgcache.DefaultCleanupInterval),
		versionProvider: versionProvider,
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

	images := map[uint64]Image{}
	for _, selectorTerm := range nodeClass.Spec.ImageSelectorTerms {
		var ims Images
		if selectorTerm.Alias != "" {
			familyProvider := p.getImageFamilyProvider(selectorTerm.Alias)
			if familyProvider == nil {
				continue
			}

			ims, err = familyProvider.ResolveImages(ctx)
			if err != nil {
				return nil, err
			}
		}

		for _, im := range ims {
			reqsHash := lo.Must(hashstructure.Hash(im.Requirements.NodeSelectorRequirements(),
				hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true}))
			// So, this means, the further ahead, the higher the priority.
			if _, ok := images[reqsHash]; ok {
				continue
			}
			images[reqsHash] = im
		}
	}

	p.cache.SetDefault(fmt.Sprintf("%d", hash), Images(lo.Values(images)))
	return lo.Values(images), nil
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
