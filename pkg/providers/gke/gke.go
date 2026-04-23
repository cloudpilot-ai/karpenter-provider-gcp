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
	"strings"
	"time"

	containerapiv1 "cloud.google.com/go/container/apiv1"
	"github.com/patrickmn/go-cache"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
)

const (
	zoneCacheExpiration      = 5 * time.Minute
	zoneCacheCleanupInterval = 1 * time.Minute
	clusterCacheTTL          = 30 * time.Minute

	zoneCacheKey    = "zone-cache"
	clusterCacheKey = "cluster"
)

type Provider interface {
	ResolveClusterZones(ctx context.Context) ([]string, error)
	GetClusterConfig(ctx context.Context) (*containerv1.Cluster, error)
}

type DefaultProvider struct {
	gkeClient        *containerapiv1.ClusterManagerClient
	computeService   *compute.Service
	containerService *containerv1.Service

	projectID    string
	nodeLocation string
	clusterName  string

	zoneCache    *cache.Cache
	clusterCache *cache.Cache
}

func NewDefaultProvider(gkeClient *containerapiv1.ClusterManagerClient, computeService *compute.Service,
	containerService *containerv1.Service, projectID, nodeLocation, clusterName string) Provider {
	return &DefaultProvider{
		gkeClient:        gkeClient,
		computeService:   computeService,
		containerService: containerService,
		projectID:        projectID,
		nodeLocation:     nodeLocation,
		clusterName:      clusterName,
		zoneCache:        cache.New(zoneCacheExpiration, zoneCacheCleanupInterval),
		clusterCache:     cache.New(clusterCacheTTL, clusterCacheTTL),
	}
}

func (p *DefaultProvider) ResolveClusterZones(ctx context.Context) ([]string, error) {
	zone, ok := p.zoneCache.Get(zoneCacheKey)
	if ok {
		return zone.([]string), nil
	}

	projectID := options.FromContext(ctx).ProjectID
	clusterLocation := options.FromContext(ctx).ClusterLocation

	region := clusterLocation
	if strings.Count(clusterLocation, "-") == 2 {
		parts := strings.Split(clusterLocation, "-")
		region = strings.Join(parts[:2], "-")
	}

	var zones []string
	prefix := region + "-"
	err := p.computeService.Zones.List(projectID).Pages(ctx, func(page *compute.ZoneList) error {
		for _, z := range page.Items {
			if strings.HasPrefix(z.Name, prefix) {
				zones = append(zones, z.Name)
			}
		}
		return nil
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "error listing zones from GCP")
		return nil, err
	}

	if len(zones) == 0 {
		return nil, fmt.Errorf("no zones found for region: %s", region)
	}

	p.zoneCache.Set(zoneCacheKey, zones, cache.DefaultExpiration)
	return zones, nil
}

func (p *DefaultProvider) GetClusterConfig(ctx context.Context) (*containerv1.Cluster, error) {
	if v, ok := p.clusterCache.Get(clusterCacheKey); ok {
		return v.(*containerv1.Cluster), nil
	}
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", p.projectID, p.nodeLocation, p.clusterName)
	cluster, err := p.containerService.Projects.Locations.Clusters.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("fetching cluster config: %w", err)
	}
	p.clusterCache.SetDefault(clusterCacheKey, cluster)
	return cluster, nil
}
