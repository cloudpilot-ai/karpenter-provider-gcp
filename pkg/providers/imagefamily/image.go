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
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	versionprovider "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

// imageResolutionError marks a resolution failure the reconciler reports as a
// status condition instead of returning for controller-runtime exponential
// backoff: either a NodeClass configuration error (invalid version format,
// unsupported family or architecture) or an Ubuntu catalog discovery miss with
// no usable image for an architecture. An Ubuntu catalog miss is not a
// configuration error and can clear on its own once GCP publishes the image.
type imageResolutionError struct{ msg string }

func (e *imageResolutionError) Error() string { return e.msg }

// IsImageResolutionError reports whether err is a resolution failure rather than
// a transient infrastructure failure. It covers user-controlled NodeClass
// configuration errors, and also Ubuntu catalog discovery misses, which can
// resolve without a configuration change once the image is published.
func IsImageResolutionError(err error) bool {
	var e *imageResolutionError
	return errors.As(err, &e)
}

// imageUnavailableError marks a valid selector with no usable image currently published, for
// example while COS images roll out variant by variant or before a promoted GKE build or pinned
// milestone has a READY image. It is neither a configuration error nor an infrastructure failure:
// the next refresh may resolve it without any user action, so the reconciler publishes
// ImagesReady=False and retries shortly instead of backing off.
type imageUnavailableError struct{ msg string }

func (e *imageUnavailableError) Error() string { return e.msg }

// IsImageUnavailableError reports whether err means no usable image is currently published for
// an otherwise valid selector.
func IsImageUnavailableError(err error) bool {
	var e *imageUnavailableError
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

// List resolves the NodeClass image selector terms. A non-empty usable result is cached per
// selector for imageCacheTTL so a staggered publication is picked up on a later refresh; errors
// and empty results are not cached.
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

	if len(result) > 0 {
		p.cache.SetDefault(cacheKey, result)
	}
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
	return p.ensureImagesAvailable(ctx, family, version, ims)
}

func (p *DefaultProvider) resolveIDTerm(ctx context.Context, term v1alpha1.ImageSelectorTerm) (Images, error) {
	img, err := p.resolveImage(ctx, term.ID)
	if err != nil {
		return nil, fmt.Errorf("resolving image %q: %w", term.ID, err)
	}
	if img == nil {
		return nil, nil
	}
	if err := imageReady(img); err != nil {
		return nil, err
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
	return p.ensureImagesAvailable(ctx, term.Family, version, ims)
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

// ensureImagesAvailable checks the images a family resolved against the Compute API. COS "latest"
// already validated every candidate while ranking the catalog, and its exact-build and milestone
// selectors derive the arm64 and NVIDIA sibling names from one matched build, so a sibling GCP has
// not published yet is omitted instead of failing the whole term. Every other family (Ubuntu, raw
// image IDs) must resolve a complete set.
func (p *DefaultProvider) ensureImagesAvailable(ctx context.Context, family, version string, ims Images) (Images, error) {
	if family != v1alpha1.ImageFamilyContainerOptimizedOS {
		return p.filterExistingImages(ctx, ims)
	}
	if version == "latest" {
		return ims, nil
	}
	return p.filterUnavailableImages(ctx, ims)
}

// filterExistingImages keeps the images that exist and fails the term when any resolved image is
// not READY. Families whose set must be complete (Ubuntu, raw IDs) use it.
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
		if err := imageReady(gceim); err != nil {
			return nil, err
		}
		result = append(result, im)
	}
	if len(ims) > 0 && len(result) == 0 {
		return nil, &imageResolutionError{msg: fmt.Sprintf("%d candidate image(s) were resolved but none exist in GCP; verify the pinned version is available", len(ims))}
	}
	return result, nil
}

// filterUnavailableImages drops the images that are missing or not READY. COS derives its arm64
// and NVIDIA sibling names from one matched build, so a sibling GCP has not published yet is
// omitted without substituting another build or version; at least one image must remain, otherwise
// the exact target is unpublished rather than misconfigured.
func (p *DefaultProvider) filterUnavailableImages(ctx context.Context, ims Images) (Images, error) {
	var result Images
	for _, im := range ims {
		gceim, err := p.resolveImage(ctx, im.SourceImage)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to resolve image", "imageSource", im.SourceImage)
			return nil, err
		}
		if gceim == nil || !imageUsable(gceim) {
			continue
		}
		result = append(result, im)
	}
	if len(result) == 0 {
		return nil, &imageUnavailableError{msg: fmt.Sprintf("%d derived COS image(s) were checked but none exist and are READY in GCP yet", len(ims))}
	}
	return result, nil
}

// imageReady requires a READY image. A listed image that is still PENDING is a publication lag, not
// an infrastructure failure, so the error is classified as unavailable and retried shortly.
func imageReady(img *compute.Image) error {
	if img.Status != "READY" {
		return &imageUnavailableError{msg: fmt.Sprintf("image %s is not ready (status: %s)", img.Name, img.Status)}
	}
	return nil
}

// imageUsable reports whether img can back a node: GCP requires READY before an image can be
// used, and this provider also excludes deprecated images that GCP would still allow.
func imageUsable(img *compute.Image) bool {
	if img.Status != "READY" {
		return false
	}
	if img.Deprecated == nil {
		return true
	}
	switch img.Deprecated.State {
	case "DEPRECATED", "OBSOLETE", "DELETED":
		return false
	}
	return true
}

func (p *DefaultProvider) resolveImage(ctx context.Context, sourceImage string) (*compute.Image, error) {
	projectID, imageName, err := parseImageSource(sourceImage)
	if err != nil {
		return nil, err
	}
	return lookupImage(ctx, p.computeService, projectID, imageName)
}

// lookupImage fetches one image by project and name. A typed 404 response is reported as
// (nil, nil) because the image does not exist; every other failure, including permission,
// rate-limit, server, and transport errors, is returned so callers cannot mistake it for absence.
func lookupImage(ctx context.Context, service *compute.Service, projectID, imageName string) (*compute.Image, error) {
	image, err := service.Images.Get(projectID, imageName).Context(ctx).Do()
	if err != nil {
		if isImageNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return image, nil
}

// isImageNotFound reports whether err is the Compute API's typed 404 response.
func isImageNotFound(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound
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
