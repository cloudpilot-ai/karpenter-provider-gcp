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
	"strings"

	k8sversion "k8s.io/apimachinery/pkg/util/version"

	versionprovider "github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/version"
)

// Modern GKE names concatenate major, two-digit minor, and patch. Do not apply
// this convention to arbitrary custom-project names or ambiguous older keys.
var cosReleaseName = regexp.MustCompile(`^gke-([1-9])([1-9][0-9])(0|[1-9][0-9]*)-gke([0-9]+)-cos-`)
var officialCOSName = regexp.MustCompile(`^gke-[0-9]+-gke[0-9]+-cos-`)

func cosRelease(sourceImage string) (string, bool) {
	project, name, err := parseImageSource(sourceImage)
	if err != nil || project != cosImageProject || !officialCOSName.MatchString(name) {
		return "", false
	}
	parts := cosReleaseName.FindStringSubmatch(name)
	if parts == nil {
		return "", true
	}
	return fmt.Sprintf("%s.%s.%s-gke.%s", parts[1], parts[2], parts[3], parts[4]), true
}

func (p *DefaultProvider) setBootstrapVersions(ctx context.Context, images Images, masterVersion string) (string, error) {
	for index := range images {
		if images[index].KubernetesVersion != "" {
			continue
		}
		if release, official := cosRelease(images[index].SourceImage); official {
			if release == "" {
				return masterVersion, &imageResolutionError{msg: "cannot decode the Kubernetes release of the selected official COS image"}
			}
			images[index].KubernetesVersion = release
			continue
		}
		// Opaque custom IDs keep their image identity but use one master snapshot.
		if masterVersion == "" {
			var err error
			masterVersion, err = bootstrapVersion(ctx, p.versionProvider)
			if err != nil {
				return "", err
			}
		}
		images[index].KubernetesVersion = masterVersion
	}
	return masterVersion, nil
}

func bootstrapVersion(ctx context.Context, provider versionprovider.Provider) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("API-server version is required to resolve the bootstrap release")
	}
	value, err := provider.Get(ctx)
	if err != nil {
		return "", fmt.Errorf("getting API-server bootstrap version: %w", err)
	}
	if _, err := k8sversion.ParseGeneric(value); err != nil {
		return "", fmt.Errorf("parsing API-server bootstrap version: %w", err)
	}
	return strings.TrimPrefix(value, "v"), nil
}
