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
	"strings"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	containerv1 "google.golang.org/api/container/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func (e *Environment) GetCluster(ctx context.Context) (*containerv1.Cluster, error) {
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", e.ProjectID, e.ClusterLocation, e.ClusterName)
	return e.containerSvc.Projects.Locations.Clusters.Get(name).Context(ctx).Do()
}

func (e *Environment) GetNodeClass(ctx context.Context, name string) (*gcpv1alpha1.GCENodeClass, error) {
	obj, err := e.DynamicClient.Resource(gceNodeClassGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	nodeClass := &gcpv1alpha1.GCENodeClass{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, nodeClass); err != nil {
		return nil, err
	}
	return nodeClass, nil
}

// GetClusterSubnetwork reads capacity independently of the controller's provider and cache.
func (e *Environment) GetClusterSubnetwork(ctx context.Context, cluster *containerv1.Cluster) (*computepb.Subnetwork, error) {
	if cluster.NetworkConfig == nil {
		return nil, fmt.Errorf("cluster has no network config")
	}
	ref := cluster.NetworkConfig.Subnetwork
	index := strings.Index(ref, "projects/")
	if index < 0 {
		return nil, fmt.Errorf("expected a fully qualified cluster subnetwork, got %q", ref)
	}
	parts := strings.Split(ref[index:], "/")
	if len(parts) != 6 || parts[2] != "regions" || parts[4] != "subnetworks" {
		return nil, fmt.Errorf("invalid cluster subnetwork %q", ref)
	}
	client, err := compute.NewSubnetworksRESTClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.Get(ctx, &computepb.GetSubnetworkRequest{
		Project: parts[1], Region: parts[3], Subnetwork: parts[5], Views: ptr.To("WITH_UTILIZATION"),
	})
}
