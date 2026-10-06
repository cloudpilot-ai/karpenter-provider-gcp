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
	"context"
	"errors"
	"slices"
	"sort"
	"strings"

	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/subnet"
)

// resolvedPodRangeNames returns the pod secondary range names to try at launch:
// Explicit NodeClass ranges replace the default and additional cluster ranges.
// A single empty string leaves SubnetworkRangeName unset so GKE can pick.
func resolvedPodRangeNames(nodeClass *v1alpha1.GCENodeClass, cluster *containerv1.Cluster) []string {
	if names := nodeClass.PodSubnetRangeNames(); len(names) > 0 {
		return names
	}
	if names := gke.ClusterPodRangeNames(cluster); len(names) > 0 {
		return names
	}
	return []string{""}
}

// rankPodRangeNames prefers the greatest known free-IP count. Unknown counts
// sort after known values; original candidate order breaks ties.
func rankPodRangeNames(names []string, freeIPs map[string]int64) []string {
	ordered := slices.Clone(names)
	sort.SliceStable(ordered, func(i, j int) bool {
		first, firstKnown := freeIPs[ordered[i]]
		second, secondKnown := freeIPs[ordered[j]]
		if firstKnown != secondKnown {
			return firstKnown
		}
		return first > second
	})
	return ordered
}

// isRejectedPodRange accepts only an explicit field-specific range rejection,
// not generic bad requests or unrelated network configuration errors.
func isRejectedPodRange(err error, rangeName string) bool {
	var apiError *googleapi.Error
	if rangeName == "" || !errors.As(err, &apiError) || apiError.Code != 400 {
		return false
	}
	for _, detail := range apiError.Errors {
		if detail.Reason != "invalid" && detail.Reason != "invalidParameter" {
			continue
		}
		if rejectedPodRangeMessage(detail.Message, rangeName) {
			return true
		}
	}
	return false
}

func rejectedPodRangeMessage(message, rangeName string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "aliasipranges") && strings.Contains(lower, "subnetworkrangename") &&
		(strings.Contains(message, "'"+rangeName+"'") || strings.Contains(message, "\""+rangeName+"\"")) &&
		(strings.Contains(lower, "not found") || strings.Contains(lower, "not valid") || strings.Contains(lower, "does not exist"))
}

func setPrimaryAliasRange(instance *compute.Instance, rangeName string) {
	if instance == nil || len(instance.NetworkInterfaces) == 0 {
		return
	}
	iface := instance.NetworkInterfaces[0]
	if len(iface.AliasIpRanges) == 0 {
		return
	}
	iface.AliasIpRanges[0].SubnetworkRangeName = rangeName
}

func (p *DefaultProvider) invalidatePodRangeCaches(instance *compute.Instance, discovery bool) {
	if len(instance.NetworkInterfaces) > 0 && instance.NetworkInterfaces[0] != nil {
		iface := instance.NetworkInterfaces[0]
		p.subnetProvider.Invalidate(iface.Network, iface.Subnetwork)
	}
	if discovery {
		p.gkeProvider.InvalidateClusterConfig()
	}
}

func (p *DefaultProvider) podRangeFreeIPs(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) map[string]int64 {
	cluster, err := p.gkeProvider.GetClusterConfig(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "getting cluster config for pod range capacity")
		return nil
	}
	names := resolvedPodRangeNames(nodeClass, cluster)
	if len(names) < 2 {
		return nil
	}
	network, target := subnet.PrimaryNetwork(nodeClass, cluster)
	if target == "" {
		return nil
	}
	counts, err := p.subnetProvider.GetFreeIPCounts(ctx, network, target)
	if err != nil {
		log.FromContext(ctx).Error(err, "getting pod range free IPs, continuing without capacity ranking", "subnetwork", target)
		return nil
	}
	return counts
}
