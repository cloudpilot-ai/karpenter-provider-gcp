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

package subnet

import (
	containerv1 "google.golang.org/api/container/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// PrimaryNetwork resolves the primary-interface network and subnetwork used for
// both instance creation and capacity reads.
func PrimaryNetwork(nodeClass *v1alpha1.GCENodeClass, cluster *containerv1.Cluster) (string, string) {
	var network, subnetwork string
	if cluster != nil && cluster.NetworkConfig != nil {
		network, subnetwork = cluster.NetworkConfig.Network, cluster.NetworkConfig.Subnetwork
	}
	if nodeClass != nil && nodeClass.Spec.NetworkConfig != nil && nodeClass.Spec.NetworkConfig.Subnetwork != "" {
		subnetwork = nodeClass.Spec.NetworkConfig.Subnetwork
	}
	return network, subnetwork
}
