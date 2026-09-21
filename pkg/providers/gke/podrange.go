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
	containerv1 "google.golang.org/api/container/v1"
)

// DefaultPodRangeName returns the secondary range GKE assigns pod IPs from by
// default, or an empty string when the cluster does not report one.
func DefaultPodRangeName(cluster *containerv1.Cluster) string {
	if cluster == nil || cluster.IpAllocationPolicy == nil {
		return ""
	}
	return cluster.IpAllocationPolicy.ClusterSecondaryRangeName
}

// PodRangeUtilization returns GKE-reported utilization for a secondary range when
// the cluster object includes it.
func PodRangeUtilization(cluster *containerv1.Cluster, name string) (float64, bool) {
	if cluster == nil || cluster.IpAllocationPolicy == nil || name == "" {
		return 0, false
	}
	pol := cluster.IpAllocationPolicy
	if name == pol.ClusterSecondaryRangeName {
		return pol.DefaultPodIpv4RangeUtilization, true
	}
	if pol.AdditionalPodRangesConfig == nil {
		return 0, false
	}
	for _, info := range pol.AdditionalPodRangesConfig.PodRangeInfo {
		if info != nil && info.RangeName == name {
			return info.Utilization, true
		}
	}
	return 0, false
}
