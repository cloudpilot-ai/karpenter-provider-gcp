/*
Copyright 2026 The CloudPilot AI Authors.

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
	"strconv"
	"strings"

	"google.golang.org/api/compute/v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// Only ordinary CPU COS and published sandbox GPU roles are supported. A COS
// override or a copied image with a convincing name is not public provenance.
var sandboxCOSName = regexp.MustCompile(`^gke-([0-9]+)-gke([0-9]+)-cos(-arm64)?-([0-9]+-[0-9]+-[0-9]+-[0-9]+)-(c-pre|(?:pre|c|c-pre)-(?:gv|gvisor)-nvda)$`)

func (p *DefaultProvider) resolveSandboxTerm(ctx context.Context, term v1alpha1.ImageSelectorTerm) (Images, error) {
	if term.ID != "" {
		return p.resolveSandboxID(ctx, term.ID)
	}
	var version string
	var err error
	if term.Alias != "" { //nolint:staticcheck
		parts := strings.SplitN(term.Alias, "@", 2) //nolint:staticcheck
		if len(parts) != 2 {
			return nil, &imageResolutionError{msg: "sandbox alias requires ContainerOptimizedOS@version"}
		}
		version = parts[1]
	} else {
		version, err = p.resolveVersion(ctx, term)
		if err != nil {
			return nil, err
		}
	}
	return p.resolveSandboxCatalog(ctx, version)
}

func (p *DefaultProvider) resolveSandboxID(ctx context.Context, id string) (Images, error) {
	project, name, err := parseImageSource(id)
	if err != nil || project != cosImageProject {
		return nil, &imageResolutionError{msg: "GVISOR explicit IDs must reference actual public GKE COS images in gke-node-images"}
	}
	img, err := p.resolveImage(ctx, id)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, &imageResolutionError{msg: "sandbox image ID does not exist"}
	}
	if img.Name != name {
		return nil, &imageResolutionError{msg: "sandbox image ID response does not match the requested public image"}
	}
	return p.sandboxImages(ctx, []*compute.Image{img})
}

func (p *DefaultProvider) resolveSandboxCatalog(ctx context.Context, version string) (Images, error) {
	cos := p.providers[v1alpha1.ImageFamilyContainerOptimizedOS].(*ContainerOptimizedOS)
	// Reuse ordinary CPU/channel resolution to determine the exact GKE build.
	// Only the anchor is used; GPU and Arm names are discovered, never generated.
	sourceImage, err := cos.resolveSourceImage(ctx, version)
	if err != nil {
		return nil, err
	}
	_, anchorName, err := parseImageSource(sourceImage)
	if err != nil {
		return nil, err
	}
	anchor := sandboxCOSName.FindStringSubmatch(anchorName)
	if anchor == nil {
		return nil, &imageResolutionError{msg: "unsupported public COS anchor for sandbox"}
	}
	var catalog []*compute.Image
	selectedRoles := map[string]bool{}
	err = scanImages(ctx, p.computeService, cosImageProject, func(img *compute.Image) bool {
		if !matchesSandboxSelection(img, anchor, cosVersionRe.MatchString(version)) {
			return false
		}
		// scanImages is newest-first. Resolve only the preferred variant per
		// architecture/role; unused old revisions must not become dependencies.
		m := sandboxCOSName.FindStringSubmatch(img.Name)
		role := m[3] + ":gpu=" + strconv.FormatBool(m[5] != "c-pre")
		if selectedRoles[role] {
			return false
		}
		selectedRoles[role] = true
		catalog = append(catalog, img)
		return false
	})
	if err != nil {
		return nil, err
	}
	if len(catalog) == 0 {
		return nil, &imageResolutionError{msg: fmt.Sprintf("no public sandbox COS images for exact GKE/COS selection %q", version)}
	}
	return p.sandboxImages(ctx, catalog)
}

func matchesSandboxSelection(img *compute.Image, anchor []string, cosPinned bool) bool {
	m := sandboxCOSName.FindStringSubmatch(img.Name)
	if m == nil || m[1] != anchor[1] || m[2] != anchor[2] {
		return false
	}
	// CPU nodes use the chosen ordinary COS build. GPU nodes can use their
	// dedicated COS build unless the user explicitly pinned a COS version.
	if (m[5] == "c-pre" || cosPinned) && m[4] != anchor[4] {
		return false
	}
	return img.Status == "READY" && !deprecatedImage(img)
}

func deprecatedImage(img *compute.Image) bool {
	return img.Deprecated != nil && (img.Deprecated.State == "DEPRECATED" || img.Deprecated.State == "OBSOLETE" || img.Deprecated.State == "DELETED")
}

func (p *DefaultProvider) sandboxImages(ctx context.Context, catalog []*compute.Image) (Images, error) {
	var result Images
	for _, img := range catalog {
		m := sandboxCOSName.FindStringSubmatch(img.Name)
		if m == nil || img.Status != "READY" || deprecatedImage(img) {
			return nil, &imageResolutionError{msg: fmt.Sprintf("image %q is not a READY supported public GKE COS sandbox image", img.Name)}
		}
		arch := OSArchAMD64Requirement
		wantArch := OSArchitectureX86
		if m[3] != "" {
			arch = OSArchARM64Requirement
			wantArch = OSArchitectureARM
		}
		if img.Architecture != wantArch {
			return nil, &imageResolutionError{msg: fmt.Sprintf("sandbox image %q architecture does not match its public COS role", img.Name)}
		}
		installer, err := p.installerResolver.ResolveInstaller(ctx, img.Labels["gvisor-version"], arch)
		if err != nil {
			return nil, fmt.Errorf("sandbox image %s: %w", img.Name, err)
		}
		gpuOp := v1.NodeSelectorOpDoesNotExist
		if m[5] != "c-pre" {
			gpuOp = v1.NodeSelectorOpExists
		}
		result = append(result, Image{SourceImage: fmt.Sprintf("projects/%s/global/images/%s", cosImageProject, img.Name), SandboxInstaller: installer, Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, arch),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, gpuOp),
		)})
	}
	return result, nil
}

// SandboxImageRelease reads the exact public image build used for compatibility
// checks. Image resolution separately verifies READY status and provenance.
func SandboxImageRelease(sourceImage string) (string, error) {
	project, name, err := parseImageSource(sourceImage)
	m := sandboxCOSName.FindStringSubmatch(name)
	if err != nil || project != cosImageProject || m == nil || len(m[1]) < 4 || m[1][0] != '1' {
		return "", fmt.Errorf("sandbox requires a supported public GKE COS image")
	}
	minor, _ := strconv.Atoi(m[1][1:3])
	patch, err := strconv.Atoi(m[1][3:])
	if err != nil {
		return "", fmt.Errorf("invalid public GKE image build")
	}
	return fmt.Sprintf("1.%d.%d-gke.%s", minor, patch, m[2]), nil
}
