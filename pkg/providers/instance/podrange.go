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

package instance

import (
	"sort"

	containerv1 "google.golang.org/api/container/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
)

// resolvedPodRangeNames returns the pod secondary range names to try at launch:
// NodeClass list or scalar, else the cluster default. A single empty string means
// leave SubnetworkRangeName unset so GKE can pick.
func resolvedPodRangeNames(nodeClass *v1alpha1.GCENodeClass, cluster *containerv1.Cluster) []string {
	if names := nodeClass.PodSubnetRangeNames(); len(names) > 0 {
		return names
	}
	return []string{gke.DefaultPodRangeName(cluster)}
}

// rankPodRangeNames orders names by lowest known GKE utilization. Names without
// utilization sort after known values; spec order is the tie-breaker.
func rankPodRangeNames(names []string, cluster *containerv1.Cluster) []string {
	type ranked struct {
		name  string
		util  float64
		known bool
		index int
	}
	items := make([]ranked, len(names))
	for i, name := range names {
		util, known := gke.PodRangeUtilization(cluster, name)
		items[i] = ranked{name: name, util: util, known: known, index: i}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].known != items[j].known {
			return items[i].known
		}
		if items[i].known && items[i].util != items[j].util {
			return items[i].util < items[j].util
		}
		return items[i].index < items[j].index
	})
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.name
	}
	return out
}
