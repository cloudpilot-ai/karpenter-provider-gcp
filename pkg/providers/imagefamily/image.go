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
	"sync"
	"time"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"google.golang.org/api/compute/v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	versionprovider "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

// imageResolutionError marks a non-transient failure caused by the NodeClass
// configuration (invalid alias version, pinned version absent from GCP).
// The reconciler surfaces these as a status condition instead of returning
// them for controller-runtime exponential backoff.
type imageResolutionError struct{ msg string }

func (e *imageResolutionError) Error() string { return e.msg }

// IsImageResolutionError reports whether err originated from user-controlled
// NodeClass configuration rather than a transient infrastructure failure.
func IsImageResolutionError(err error) bool {
	var e *imageResolutionError
	return errors.As(err, &e)
}

const (
	imageCacheTTL             = 5 * time.Minute
	imageCacheCleanupInterval = time.Minute
)

type Provider interface {
	List(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (Images, error)
}

type DefaultProvider struct {
	sync.Mutex
	cache           *cache.Cache
	computeService  *compute.Service
	gkeProvider     gke.Provider
	versionProvider versionprovider.Provider

	providers map[string]ImageFamily
}

// NewDefaultProvider creates the image provider. gkeProvider is used for channel-based
// resolution; pass a no-op implementation if channel: terms are not needed.
func NewDefaultProvider(computeService *compute.Service, versionProvider versionprovider.Provider, gkeProvider gke.Provider) *DefaultProvider {
	cos := &ContainerOptimizedOS{computeService: computeService, versionProvider: versionProvider}
	ubuntu2404 := &Ubuntu{computeService: computeService, versionProvider: versionProvider, release: "2404"}
	ubuntu2204 := &Ubuntu{computeService: computeService, versionProvider: versionProvider, release: "2204"}
	return &DefaultProvider{
		cache:           cache.New(imageCacheTTL, imageCacheCleanupInterval),
		computeService:  computeService,
		gkeProvider:     gkeProvider,
		versionProvider: versionProvider,
		providers: map[string]ImageFamily{
			v1alpha1.ImageFamilyContainerOptimizedOS: cos,
			v1alpha1.ImageFamilyUbuntu2404:           ubuntu2404,
			v1alpha1.ImageFamilyUbuntu:               ubuntu2404, // legacy alias path
			v1alpha1.ImageFamilyUbuntu2204:           ubuntu2204,
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
	cacheKey := fmt.Sprintf("%d", hash)
	if images, ok := p.cache.Get(cacheKey); ok {
		return images.(Images), nil
	}

	var result Images
	for _, term := range nodeClass.Spec.ImageSelectorTerms {
		imgs, err := p.resolveImageFromTerm(ctx, term)
		if err != nil {
			return nil, err
		}
		result = append(result, imgs...)
	}

	p.cache.SetDefault(cacheKey, result)
	return result, nil
}

func (p *DefaultProvider) resolveImageFromTerm(ctx context.Context, term v1alpha1.ImageSelectorTerm) (Images, error) {
	switch {
	case term.Alias != "": //nolint:staticcheck
		return p.resolveAliasTerm(ctx, term)
	case term.ID != "":
		return p.resolveIDTerm(ctx, term)
	case term.Family != "":
		return p.resolveFamilyTerm(ctx, term)
	default:
		return nil, fmt.Errorf("ImageSelectorTerm has no selection field (alias, id, or family)")
	}
}

func (p *DefaultProvider) resolveAliasTerm(ctx context.Context, term v1alpha1.ImageSelectorTerm) (Images, error) {
	//nolint:staticcheck
	components := strings.SplitN(term.Alias, "@", 2)
	if len(components) != 2 {
		//nolint:staticcheck
		return nil, fmt.Errorf("malformed alias %q: expected family@version", term.Alias)
	}
	family, version := components[0], components[1]
	provider := p.getImageFamilyProvider(family)
	if provider == nil {
		return nil, fmt.Errorf("unsupported alias family %q", family)
	}
	ims, err := provider.ResolveImages(ctx, version)
	if err != nil {
		return nil, err
	}
	return p.filterExistingImages(ctx, ims)
}

func (p *DefaultProvider) resolveIDTerm(ctx context.Context, term v1alpha1.ImageSelectorTerm) (Images, error) {
	img, err := p.resolveImage(ctx, term.ID)
	if err != nil {
		return nil, fmt.Errorf("resolving image %q: %w", term.ID, err)
	}
	if img == nil {
		return nil, nil
	}

	var requirements scheduling.Requirements
	switch img.Architecture {
	case OSArchitectureX86:
		requirements = scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchAMD64Requirement),
		)
	case OSArchitectureARM:
		requirements = scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, OSArchARM64Requirement),
		)
	default:
		return nil, fmt.Errorf("unsupported architecture %q for image %q", img.Architecture, term.ID)
	}
	return Images{{SourceImage: term.ID, Requirements: requirements}}, nil
}

func (p *DefaultProvider) resolveFamilyTerm(ctx context.Context, term v1alpha1.ImageSelectorTerm) (Images, error) {
	version, err := p.resolveVersion(ctx, term)
	if err != nil {
		return nil, err
	}
	provider := p.getImageFamilyProvider(term.Family)
	if provider == nil {
		return nil, fmt.Errorf("unsupported family %q", term.Family)
	}
	ims, err := provider.ResolveImages(ctx, version)
	if err != nil {
		return nil, err
	}
	return p.filterExistingImages(ctx, ims)
}

// resolveVersion returns the version string to pass to the image family provider.
// For channel terms it fetches serverConfig and resolves the build version;
// for version terms it returns the version string directly.
func (p *DefaultProvider) resolveVersion(ctx context.Context, term v1alpha1.ImageSelectorTerm) (string, error) {
	if term.Version != "" {
		return term.Version, nil
	}

	channelName := term.Channel
	if channelName == v1alpha1.ImageChannelCluster {
		cluster, err := p.gkeProvider.GetClusterConfig(ctx)
		if err != nil {
			return "", fmt.Errorf("GetClusterConfig for channel: cluster: %w", err)
		}
		if cluster.ReleaseChannel == nil ||
			cluster.ReleaseChannel.Channel == "" ||
			cluster.ReleaseChannel.Channel == "UNSPECIFIED" {
			return "", &imageResolutionError{msg: "cluster has no enrolled release channel (UNSPECIFIED); " +
				"channel: cluster requires an enrolled channel — " +
				"enroll the cluster in a release channel or use version: latest explicitly"}
		}
		channelName = cluster.ReleaseChannel.Channel
	}

	sc, err := p.gkeProvider.GetServerConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("GetServerConfig for channel resolution: %w", err)
	}
	clusterVersion, err := p.versionProvider.Get(ctx)
	if err != nil {
		return "", fmt.Errorf("getting cluster version for channel resolution: %w", err)
	}
	version, err := gke.ResolveVersionForChannel(sc, channelName, clusterVersion)
	if err != nil {
		return "", &imageResolutionError{msg: err.Error()}
	}
	return version, nil
}

func (p *DefaultProvider) getImageFamilyProvider(family string) ImageFamily {
	return p.providers[family]
}

func (p *DefaultProvider) filterExistingImages(ctx context.Context, ims Images) (Images, error) {
	var result Images
	for _, im := range ims {
		gceim, err := p.resolveImage(ctx, im.SourceImage)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to resolve image", "imageSource", im.SourceImage)
			return nil, err
		}
		if gceim == nil {
			continue
		}
		result = append(result, im)
	}
	if len(ims) > 0 && len(result) == 0 {
		return nil, &imageResolutionError{msg: fmt.Sprintf("%d candidate image(s) were resolved but none exist in GCP; verify the pinned version is available", len(ims))}
	}
	return result, nil
}

func (p *DefaultProvider) resolveImage(ctx context.Context, sourceImage string) (*compute.Image, error) {
	projectID, imageName, err := parseImageSource(sourceImage)
	if err != nil {
		return nil, err
	}

	image, err := p.computeService.Images.Get(projectID, imageName).Context(ctx).Do()
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
