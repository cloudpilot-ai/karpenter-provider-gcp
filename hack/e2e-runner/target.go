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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
)

type lifecycleTarget struct {
	Project, Location, Region, Prefix, Cluster, Repository, ControllerSA, NodeSA string
	systemGroups                                                                 map[string]bool
	requireTestBootstrap                                                         bool
	Pods, SmallPods, LargePods, TagKey                                           string
}

func loadLifecycleTarget(prepared bool) (t lifecycleTarget, err error) {
	fields := []struct {
		key  string
		dest *string
	}{
		{"E2E_PROJECT_ID", &t.Project}, {"E2E_LOCATION", &t.Location}, {"E2E_REGION", &t.Region}, {"E2E_PREFIX", &t.Prefix}, {"E2E_CLUSTER_NAME", &t.Cluster}, {"E2E_IMAGE_REPOSITORY", &t.Repository}, {"E2E_CONTROLLER_SERVICE_ACCOUNT", &t.ControllerSA}, {"E2E_NODE_SERVICE_ACCOUNT", &t.NodeSA},
		{"E2E_PODS_RANGE_NAME", &t.Pods}, {"E2E_SMALL_PODS_RANGE_NAME", &t.SmallPods}, {"E2E_LARGE_PODS_RANGE_NAME", &t.LargePods}, {"E2E_RESOURCE_MANAGER_TAG_KEY", &t.TagKey},
	}
	for _, f := range fields {
		*f.dest = os.Getenv(f.key)
	}
	// Derivation is exclusively the explicit local/release path; CI never falls back.
	if !prepared {
		if t.Prefix == "" {
			t.Prefix = "karpenter-e2e"
		}
		if t.Region == "" {
			// The upstream Make cleanup recipe forwards location, not region.
			// GCP zones append a single letter to the regional location.
			t.Region = regexp.MustCompile(`-[a-z]$`).ReplaceAllString(t.Location, "")
		}
		if t.Cluster == "" {
			t.Cluster = t.Prefix + "-cluster"
		}
		if t.Repository == "" {
			t.Repository = t.Region + "-docker.pkg.dev/" + t.Project + "/" + t.Prefix + "-images/karpenter"
		}
		if t.ControllerSA == "" {
			t.ControllerSA = t.Prefix + "-karpenter@" + t.Project + ".iam.gserviceaccount.com"
		}
	}
	for i, f := range fields {
		if *f.dest == "" && (prepared || i < 7) {
			return t, fmt.Errorf("missing %s", f.key)
		}
	}
	simple := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	for _, value := range []string{t.Project, t.Location, t.Region, t.Prefix, t.Cluster} {
		if !simple.MatchString(value) {
			return t, fmt.Errorf("invalid target identity")
		}
	}
	if t.Cluster != t.Prefix+"-cluster" || !(t.Location == t.Region || strings.HasPrefix(t.Location, t.Region+"-")) {
		return t, fmt.Errorf("inconsistent target cluster/location")
	}
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(t.Region+"-docker.pkg.dev/"+t.Project+"/") + `[a-z0-9-]+/[a-z0-9-]+$`).MatchString(t.Repository) {
		return t, fmt.Errorf("inconsistent image repository")
	}
	for _, sa := range []string{t.ControllerSA, t.NodeSA} {
		if sa == "" && !prepared {
			continue
		}
		if !regexp.MustCompile(`^[a-z][a-z0-9-]+@` + regexp.QuoteMeta(t.Project) + `\.iam\.gserviceaccount\.com$`).MatchString(sa) {
			return t, fmt.Errorf("invalid target service account")
		}
	}
	if prepared {
		for _, value := range []string{t.Pods, t.SmallPods, t.LargePods, t.TagKey} {
			if !simple.MatchString(value) {
				return t, fmt.Errorf("invalid feature configuration (tag key must be short_name)")
			}
		}
	}
	if prepared && (t.SmallPods == t.LargePods || t.SmallPods == t.Pods || t.LargePods == t.Pods) {
		return t, fmt.Errorf("pod range keys must be distinct")
	}
	return t, nil
}

func readToolJSON(ctx context.Context, dest any, tool string, args ...string) error {
	data, err := boundedCommandOutput(ctx, tool, args...)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dest)
}

func (t *lifecycleTarget) preflight(ctx context.Context, prepared bool) error {
	t.requireTestBootstrap = prepared
	current, err := boundedCommandOutput(ctx, "kubectl", "config", "current-context")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(current)) != "gke_"+t.Project+"_"+t.Location+"_"+t.Cluster {
		return fmt.Errorf("target kubeconfig context mismatch")
	}
	var cluster struct {
		Name, Location, SelfLink, Endpoint, Subnetwork string
		NodePools                                      []struct {
			InstanceGroupURLs []string `json:"instanceGroupUrls"`
		}
		IPAllocationPolicy struct {
			ClusterSecondaryRangeName string
			AdditionalPodRangesConfig struct{ PodRangeNames []string }
		}
	}
	if err := readToolJSON(ctx, &cluster, "gcloud", "container", "clusters", "describe", t.Cluster, "--project", t.Project, "--location", t.Location, "--format=json"); err != nil {
		return err
	}
	expected := "projects/" + t.Project + "/locations/" + t.Location + "/clusters/" + t.Cluster
	// GKE's selfLink uses locations for both zonal and regional clusters.
	if cluster.Name != t.Cluster || cluster.Location != t.Location || !strings.HasSuffix(cluster.SelfLink, "/"+expected) || cluster.Endpoint == "" {
		return fmt.Errorf("actual GKE target mismatch")
	}
	var config struct {
		Clusters []struct{ Cluster struct{ Server string } }
	}
	if err := readToolJSON(ctx, &config, "kubectl", "config", "view", "--minify", "-o", "json"); err != nil {
		return err
	}
	if len(config.Clusters) != 1 || config.Clusters[0].Cluster.Server != "https://"+cluster.Endpoint {
		return fmt.Errorf("actual Kubernetes endpoint mismatch")
	}
	// Bootstrap is operational configuration, never repaired by a normal run.
	if _, err := boundedCommandOutput(ctx, "kubectl", "get", "namespace", "karpenter-system", "-o", "name"); err != nil {
		return fmt.Errorf("runtime namespace bootstrap unavailable: %w", err)
	}
	t.systemGroups = map[string]bool{}
	for _, pool := range cluster.NodePools {
		for _, group := range pool.InstanceGroupURLs {
			prefix := "https://www.googleapis.com/compute/v1/projects/" + t.Project + "/zones/"
			if !strings.HasPrefix(group, prefix) || !strings.Contains(group, "/instanceGroupManagers/") {
				return fmt.Errorf("invalid GKE bootstrap group identity")
			}
			t.systemGroups[group] = true
		}
	}
	if !prepared {
		return nil
	}
	if err := verifyTestBootstrap(ctx, *t); err != nil {
		return err
	}
	if _, err := boundedCommandOutput(ctx, "kubectl", "get", "priorityclass", "gmp-critical", "-o", "name"); err != nil {
		return fmt.Errorf("priority class bootstrap unavailable: %w", err)
	}
	if cluster.IPAllocationPolicy.ClusterSecondaryRangeName != t.Pods {
		return fmt.Errorf("primary pod range mismatch")
	}
	attached := map[string]bool{}
	for _, key := range cluster.IPAllocationPolicy.AdditionalPodRangesConfig.PodRangeNames {
		attached[key] = true
	}
	if !attached[t.SmallPods] || !attached[t.LargePods] {
		return fmt.Errorf("additional pod ranges not attached")
	}
	subnet := t.Prefix + "-subnet"
	if !strings.HasSuffix(cluster.Subnetwork, "/subnetworks/"+subnet) && cluster.Subnetwork != subnet {
		return fmt.Errorf("target subnetwork mismatch")
	}
	var network struct {
		SecondaryIPRanges []struct{ RangeName, IPCIDRRange string }
	}
	if err := readToolJSON(ctx, &network, "gcloud", "compute", "networks", "subnets", "describe", subnet, "--project", t.Project, "--region", t.Region, "--format=json"); err != nil {
		return err
	}
	ranges := map[string]netip.Prefix{}
	for _, r := range network.SecondaryIPRanges {
		cidr, err := netip.ParsePrefix(r.IPCIDRRange)
		if err != nil {
			return err
		}
		ranges[r.RangeName] = cidr
	}
	small, large := ranges[t.SmallPods], ranges[t.LargePods]
	if !ranges[t.Pods].IsValid() || !small.IsValid() || !large.IsValid() || !small.Addr().Is4() || !large.Addr().Is4() || small.Bits() <= large.Bits() || small.Bits() > 22 {
		return fmt.Errorf("pod ranges lack required small/large capacity semantics")
	}
	var key struct{ Name, ShortName string }
	if err := readToolJSON(ctx, &key, "gcloud", "resource-manager", "tags", "keys", "describe", t.Project+"/"+t.TagKey, "--format=json"); err != nil {
		return err
	}
	if key.ShortName != t.TagKey || !regexp.MustCompile(`^tagKeys/[0-9]+$`).MatchString(key.Name) {
		return fmt.Errorf("tag key configuration mismatch")
	}
	var values []struct{ Parent, ShortName string }
	if err := readToolJSON(ctx, &values, "gcloud", "resource-manager", "tags", "values", "list", "--parent", key.Name, "--format=json"); err != nil {
		return err
	}
	for _, value := range values {
		if value.Parent == key.Name && value.ShortName == "e2e" {
			return nil
		}
	}
	return fmt.Errorf("required e2e tag value unavailable")
}
