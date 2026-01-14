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

	container "cloud.google.com/go/container/apiv1"
	"github.com/patrickmn/go-cache"
	"google.golang.org/api/compute/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
)

const (
	zoneCacheExpiration      = 5 * time.Minute
	zoneCacheCleanupInterval = 1 * time.Minute

	zoneCacheKey = "zone-cache"
)

type Provider interface {
	ResolveClusterZones(ctx context.Context) ([]string, error)
}

type DefaultProvider struct {
	gkeClient      *container.ClusterManagerClient
	computeService *compute.Service

	zoneCache *cache.Cache
}

func NewDefaultProvider(gkeClient *container.ClusterManagerClient, computeService *compute.Service) Provider {
	return &DefaultProvider{
		gkeClient:      gkeClient,
		computeService: computeService,
		zoneCache:      cache.New(zoneCacheExpiration, zoneCacheCleanupInterval),
	}
}

func (p *DefaultProvider) ResolveClusterZones(ctx context.Context) ([]string, error) {
	zone, ok := p.zoneCache.Get(zoneCacheKey)
	if ok {
		return zone.([]string), nil
	}

	projectID := options.FromContext(ctx).ProjectID
	location := options.FromContext(ctx).Location

	region := location
	if strings.Count(location, "-") == 2 {
		parts := strings.Split(location, "-")
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
