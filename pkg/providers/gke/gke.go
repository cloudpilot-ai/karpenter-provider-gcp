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

package gke

import (
	"context"
	"fmt"

	container "cloud.google.com/go/container/apiv1"
	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
)

type Provider interface {
	ResolveClusterZones(ctx context.Context) ([]string, error)
}

type DefaultProvider struct {
	gkeClient *container.ClusterManagerClient
}

func NewDefaultProvider(gkeClient *container.ClusterManagerClient) Provider {
	return &DefaultProvider{
		gkeClient: gkeClient,
	}
}

func (p *DefaultProvider) ResolveClusterZones(ctx context.Context) ([]string, error) {
	projectID := options.FromContext(ctx).ProjectID
	clusterName := options.FromContext(ctx).ClusterName
	resp, err := p.gkeClient.ListClusters(ctx, &containerpb.ListClustersRequest{
		ProjectId: projectID,
		Zone:      "-", // "-" means all zones
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to list clusters")
		return nil, err
	}

	targetCluster, ok := lo.Find(resp.Clusters, func(cluster *containerpb.Cluster) bool {
		return cluster.Name == clusterName
	})
	if !ok {
		return nil, fmt.Errorf("cluster %s not found", clusterName)
	}

	return targetCluster.Locations, nil
}
