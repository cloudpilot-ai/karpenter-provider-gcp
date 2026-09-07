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
	"context"
	"fmt"

	"github.com/patrickmn/go-cache"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
)

// SelfHostedProvider is the Provider for self-hosted (non-GKE) clusters: backed only by
// the Compute API, it lists zones region-wide and rejects cluster/server config lookups.
type SelfHostedProvider struct {
	computeService *compute.Service

	projectID string
	region    string

	zoneCache *cache.Cache
}

func NewSelfHostedProvider(computeService *compute.Service, projectID, region string) Provider {
	return &SelfHostedProvider{
		computeService: computeService,
		projectID:      projectID,
		region:         region,
		zoneCache:      cache.New(zoneCacheExpiration, zoneCacheCleanupInterval),
	}
}

func (p *SelfHostedProvider) ResolveClusterZones(ctx context.Context) ([]string, error) {
	if zone, ok := p.zoneCache.Get(zoneCacheKey); ok {
		return zone.([]string), nil
	}

	zones, err := listZoneNamesInRegion(ctx, p.computeService, p.projectID, p.region, true)
	if err != nil {
		return nil, err
	}
	p.zoneCache.Set(zoneCacheKey, zones, cache.DefaultExpiration)
	return zones, nil
}

func (p *SelfHostedProvider) GetClusterConfig(_ context.Context) (*containerv1.Cluster, error) {
	return nil, fmt.Errorf("cluster config is not available in self-hosted mode")
}

func (p *SelfHostedProvider) GetServerConfig(_ context.Context) (*containerv1.ServerConfig, error) {
	return nil, fmt.Errorf("server config is not available in self-hosted mode")
}
