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
	"slices"
	"strings"

	"google.golang.org/api/compute/v1"
	v1 "k8s.io/api/core/v1"
	k8sversion "k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	versionprovider "github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/version"
)

const (
	// ubuntuLegacyImageProject publishes GKE Ubuntu node images for GKE versions before 1.36.
	ubuntuLegacyImageProject = "ubuntu-os-gke-cloud"
	// ubuntuImageProject publishes them for GKE 1.36 and later.
	ubuntuImageProject = "gke-ubuntu-node-images"
	// ubuntuImageProjectMinor is the first Kubernetes 1.x minor served from ubuntuImageProject.
	ubuntuImageProjectMinor = 36
)

// ubuntuPinnedVersionRe validates a pinned image version (vYYYYMMDD, optionally with a release letter).
var ubuntuPinnedVersionRe = regexp.MustCompile(`^v\d{8}[a-z]?$`)

// ubuntuRelease holds the clean-name allowlists for one Ubuntu release. Anchoring on the version
// end rejects variant builds (-tpu, -cgroupsv1, -linux64k, -64k, -hwe, -acs, ...) while allowing
// a trailing date letter (v20251218a).
type ubuntuRelease struct {
	amd64, arm64 *regexp.Regexp
	// hint is appended to resolution failures to explain a known catalog limit.
	hint string
}

// newUbuntuRelease builds the allowlists for a release. 2204 amd64 names carry no arch token.
func newUbuntuRelease(id string, amd64ArchToken bool, hint string) ubuntuRelease {
	amd64Token := ""
	if amd64ArchToken {
		amd64Token = "amd64-"
	}
	return ubuntuRelease{
		amd64: regexp.MustCompile(fmt.Sprintf(`^ubuntu-gke-%s-\d+-\d+-%sv\d+[a-z]?$`, id, amd64Token)),
		arm64: regexp.MustCompile(fmt.Sprintf(`^ubuntu-gke-%s-\d+-\d+-arm64-v\d+[a-z]?$`, id)),
		hint:  hint,
	}
}

// ubuntuOSVersions lists the releases in ubuntuReleases as osVersion values ("22.04"), sorted.
func ubuntuOSVersions() []string {
	versions := make([]string, 0, len(ubuntuReleases))
	for release := range ubuntuReleases {
		versions = append(versions, release[:2]+"."+release[2:])
	}
	slices.Sort(versions)
	return versions
}

// ubuntuReleases is keyed by the release token embedded in image names.
var ubuntuReleases = map[string]ubuntuRelease{
	"2204": newUbuntuRelease("2204", false, "GKE publishes Ubuntu 22.04 node images only through Kubernetes 1.31"),
	"2404": newUbuntuRelease("2404", true, ""),
	"2604": newUbuntuRelease("2604", true, ""),
}

// Ubuntu resolves GKE Ubuntu images from the image project that serves the cluster's Kubernetes
// minor: ubuntu-os-gke-cloud before GKE 1.36, gke-ubuntu-node-images from 1.36.
// The release field selects the Ubuntu release: "2204", "2404" or "2604" (a key of ubuntuReleases).
type Ubuntu struct {
	computeService  *compute.Service
	versionProvider versionprovider.Provider
	release         string
}

func (u *Ubuntu) imagePrefix() string {
	return "ubuntu-gke-" + u.release
}

// architecture requirements this provider resolves an image for, in output order.
var ubuntuArchitectures = []string{OSArchAMD64Requirement, OSArchARM64Requirement}

func (u *Ubuntu) ResolveImages(ctx context.Context, version string) (Images, error) {
	if version != "latest" && !ubuntuPinnedVersionRe.MatchString(version) {
		return nil, &imageResolutionError{msg: fmt.Sprintf(
			"invalid Ubuntu version %q: must be 'latest' or 'vYYYYMMDD' with an optional release letter (e.g. 'v20260416', 'v20251218a')", version)}
	}

	kubernetesVersion, err := bootstrapVersion(ctx, u.versionProvider)
	if err != nil {
		return nil, err
	}
	project, prefix, err := u.imageScope(kubernetesVersion)
	if err != nil {
		return nil, err
	}
	images, err := u.findImages(ctx, version, project, prefix)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve Ubuntu GKE image from image project")
		return Images{}, err
	}

	// A missing architecture (incl. a pinned date absent for one arch) fails resolution instead of
	// producing a single-arch set that reads as ImagesReady=True.
	ret := Images{}
	for _, arch := range ubuntuArchitectures {
		img, ok := images[arch]
		if !ok {
			msg := fmt.Sprintf("no %s %s image found for version %q in %s",
				u.imagePrefix(), arch, version, project)
			if hint := ubuntuReleases[u.release].hint; hint != "" {
				msg += "; " + hint
			}
			return nil, &imageResolutionError{msg: msg}
		}
		ret = append(ret, Image{
			SourceImage:       fmt.Sprintf("projects/%s/global/images/%s", project, img.Name),
			KubernetesVersion: kubernetesVersion,
			Requirements:      ubuntuRequirementsForArch(arch),
		})
	}
	return ret, nil
}

// findImages returns the newest usable image for each architecture ("latest"), or the exact-date
// image for a pin. scanImages visits newest first, so the first usable match wins.
func (u *Ubuntu) findImages(ctx context.Context, version, project, prefix string) (map[string]*compute.Image, error) {
	found := make(map[string]*compute.Image, len(ubuntuArchitectures))
	err := scanImages(ctx, u.computeService, project, func(img *compute.Image) bool {
		if !strings.HasPrefix(img.Name, prefix) || (version != "latest" && !strings.HasSuffix(img.Name, "-"+version)) {
			return false
		}
		for _, arch := range ubuntuArchitectures {
			if found[arch] == nil && u.isUsableForArch(img, arch) {
				found[arch] = img
			}
		}
		return len(found) == len(ubuntuArchitectures)
	})
	if err != nil {
		return nil, fmt.Errorf("listing Ubuntu GKE images in %s: %w", project, err)
	}
	return found, nil
}

// isUsableForArch reports whether img is a READY, non-deprecated, clean image of this release for arch.
func (u *Ubuntu) isUsableForArch(img *compute.Image, arch string) bool {
	release, ok := ubuntuReleases[u.release]
	if !ok || !imageUsable(img) {
		return false
	}
	if arch == OSArchARM64Requirement {
		return release.arm64.MatchString(img.Name)
	}
	return release.amd64.MatchString(img.Name)
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

// imageScope returns the image project and name prefix for the cluster's Kubernetes minor.
// GKE publishes Ubuntu images for 1.36+ in ubuntuImageProject and for earlier minors in
// ubuntuLegacyImageProject; the projects are not mixed or used as fallbacks for each other.
func (u *Ubuntu) imageScope(kubernetesVersion string) (project, prefix string, err error) {
	parsed, err := k8sversion.ParseGeneric(kubernetesVersion)
	if err != nil {
		return "", "", fmt.Errorf("parsing Ubuntu image version: %w", err)
	}
	project = ubuntuLegacyImageProject
	if parsed.Major() > 1 || parsed.Minor() >= ubuntuImageProjectMinor {
		project = ubuntuImageProject
	}
	return project, fmt.Sprintf("%s-%d-%d", u.imagePrefix(), parsed.Major(), parsed.Minor()), nil
}

// ubuntuImageProjects lists the projects that serve GKE Ubuntu images.
var ubuntuImageProjects = []string{ubuntuLegacyImageProject, ubuntuImageProject}

// SameImage reports whether imageID, the boot image of a running node, is sourceImage from the
// NodeClass status. GKE mirrors Ubuntu images under identical names in both Ubuntu image projects,
// so a node launched from one project is not drifted by the other project serving that image.
func SameImage(imageID, sourceImage string) bool {
	if strings.HasSuffix(imageID, sourceImage) {
		return true
	}
	project, name, err := parseImageSource(sourceImage)
	if err != nil || !slices.Contains(ubuntuImageProjects, project) {
		return false
	}
	for _, other := range ubuntuImageProjects {
		if other != project && strings.HasSuffix(imageID, fmt.Sprintf("projects/%s/global/images/%s", other, name)) {
			return true
		}
	}
	return false
}
