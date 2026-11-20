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

package environment

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	k8sversion "k8s.io/apimachinery/pkg/util/version"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/imagefamily"
)

var baseCOSImage = regexp.MustCompile(`^gke-\d+-gke\d+-cos-\d+-\d+-\d+-\d+-c-pre$`)

// COSReleaseNewerThanSourcePools finds a published supported release newer than
// every possible bootstrap source, without changing pools or controller policy.
func (e *Environment) COSReleaseNewerThanSourcePools(ctx context.Context) (string, map[string]string) {
	cluster, err := e.GetCluster(ctx)
	Expect(err).NotTo(HaveOccurred(), "getting bootstrap source pool versions")
	for _, pool := range cluster.NodePools {
		if pool.Status != "RUNNING" && pool.Status != "RUNNING_WITH_ERROR" {
			Skip("bootstrap skew requires stable existing GKE source pools")
		}
	}
	sources := bootstrapSourceVersions(cluster)
	releases := make([]string, 0, len(sources))
	for _, release := range sources {
		releases = append(releases, release)
	}
	slices.Sort(releases)
	GinkgoWriter.Printf("[bootstrap skew] control-plane=%s sourceVersions=%v\n", cluster.CurrentMasterVersion, releases)
	config, err := e.containerSvc.Projects.Locations.
		GetServerConfig(fmt.Sprintf("projects/%s/locations/%s", e.ProjectID, e.ClusterLocation)).
		Context(ctx).Do()
	Expect(err).NotTo(HaveOccurred(), "getting supported GKE node versions")
	prefixes, err := newerCOSImagePrefixes(cluster.CurrentMasterVersion, sources, config.ValidNodeVersions)
	Expect(err).NotTo(HaveOccurred(), "selecting a newer same-minor bootstrap release")
	if len(prefixes) == 0 {
		Skip("bootstrap skew requires a supported same-minor GKE release newer than every source pool and not newer than the control plane")
	}

	var target string
	image, err := newestImage(ctx, e.computeSvc, "gke-node-images", func(image *compute.Image) bool {
		if !baseCOSImage.MatchString(image.Name) {
			return false
		}
		for prefix, version := range prefixes {
			if strings.HasPrefix(image.Name, prefix) {
				target = version
				return true
			}
		}
		return false
	})
	Expect(err).NotTo(HaveOccurred(), "finding a published amd64 COS skew image")
	if image == nil {
		Skip("bootstrap skew requires a READY, nondeprecated amd64 COS image for a newer supported release")
	}
	GinkgoWriter.Printf("[bootstrap skew] selected target=%s\n", target)
	return target, sources
}

// BootstrapSourceVersions snapshots all GKE pools, so the test need not duplicate
// the controller's preferred/default/fallback source-pool selection algorithm.
func (e *Environment) BootstrapSourceVersions(ctx context.Context) map[string]string {
	cluster, err := e.GetCluster(ctx)
	Expect(err).NotTo(HaveOccurred(), "getting bootstrap source pool versions")
	return bootstrapSourceVersions(cluster)
}

func bootstrapSourceVersions(cluster *container.Cluster) map[string]string {
	Expect(cluster.NodePools).NotTo(BeEmpty(), "bootstrap skew requires an existing GKE source pool")
	sources := make(map[string]string, len(cluster.NodePools))
	for _, pool := range cluster.NodePools {
		sources[pool.Name] = pool.Version
	}
	return sources
}

func newerCOSImagePrefixes(master string, sources map[string]string, supported []string) (map[string]string, error) {
	masterVersion, err := k8sversion.ParseSemantic(master)
	if err != nil {
		return nil, fmt.Errorf("parsing master version: %w", err)
	}
	newestSource, err := newestBootstrapSourceVersion(sources)
	if err != nil {
		return nil, err
	}

	prefixes := map[string]string{}
	minorPrefix := fmt.Sprintf("%d.%d.", masterVersion.Major(), masterVersion.Minor())
	for _, raw := range supported {
		version, err := k8sversion.ParseSemantic(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing supported node version: %w", err)
		}
		if version.GreaterThan(masterVersion) {
			continue
		}
		if !strings.HasPrefix(strings.TrimPrefix(raw, "v"), minorPrefix) || !version.GreaterThan(newestSource) {
			continue
		}
		key, build, ok := imagefamily.ParseGKEVersion(raw)
		if ok {
			prefixes[fmt.Sprintf("gke-%s-gke%s-cos-", key, build)] = strings.TrimPrefix(raw, "v")
		}
	}
	return prefixes, nil
}

func newestBootstrapSourceVersion(sources map[string]string) (*k8sversion.Version, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("no source pool versions")
	}
	newest := k8sversion.MustParseSemantic("0.0.0")
	for _, raw := range sources {
		version, err := k8sversion.ParseSemantic(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing source pool version: %w", err)
		}
		if version.GreaterThan(newest) {
			newest = version
		}
	}
	return newest, nil
}
