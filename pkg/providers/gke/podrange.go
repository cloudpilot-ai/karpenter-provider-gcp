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

package gke

import (
	"slices"

	containerv1 "google.golang.org/api/container/v1"
)

// ClusterPodRangeNames returns the default and additional pod ranges on the
// cluster's primary subnetwork, preserving GKE order and removing duplicates.
func ClusterPodRangeNames(cluster *containerv1.Cluster) []string {
	if cluster == nil || cluster.IpAllocationPolicy == nil {
		return nil
	}
	pol := cluster.IpAllocationPolicy
	candidates := []string{pol.ClusterSecondaryRangeName}
	if additional := pol.AdditionalPodRangesConfig; additional != nil {
		candidates = append(candidates, additional.PodRangeNames...)
		for _, info := range additional.PodRangeInfo {
			if info != nil {
				candidates = append(candidates, info.RangeName)
			}
		}
	}
	var names []string
	for _, name := range candidates {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}
