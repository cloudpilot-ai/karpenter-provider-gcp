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
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
)

const (
	lookupTimeout = 3 * time.Second
	cacheTTL      = time.Minute
	failureTTL    = 10 * time.Second
)

type Provider interface {
	GetFreeIPCounts(ctx context.Context, network, subnetwork string) (map[string]int64, error)
	Invalidate(network, subnetwork string)
}

// SubnetworksClient reads subnet utilization from the Compute API.
type SubnetworksClient interface {
	Get(context.Context, *computepb.GetSubnetworkRequest, ...gax.CallOption) (*computepb.Subnetwork, error)
}

type DefaultProvider struct {
	client SubnetworksClient
	region string
	clock  clock.PassiveClock
	mu     sync.Mutex
	cache  map[subnetworkRef]snapshot
}

type snapshot struct {
	counts    map[string]int64
	err       error
	expiresAt time.Time
}

func NewProvider(client SubnetworksClient, region string, clock clock.PassiveClock) Provider {
	return &DefaultProvider{client: client, region: region, clock: clock, cache: map[subnetworkRef]snapshot{}}
}

func (p *DefaultProvider) GetFreeIPCounts(ctx context.Context, network, subnetwork string) (map[string]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := resolveTarget(network, subnetwork, p.region)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cached, ok := p.cache[target]; ok && p.clock.Now().Before(cached.expiresAt) {
		return maps.Clone(cached.counts), cached.err
	}
	delete(p.cache, target)

	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	response, err := p.client.Get(lookupCtx, &computepb.GetSubnetworkRequest{
		Project: target.project, Region: target.region, Subnetwork: target.name,
		Views: ptr.To("WITH_UTILIZATION"),
	})
	if err != nil {
		err = fmt.Errorf("getting free IPs for %s: %w", target, err)
		// A canceled caller must not put other callers into the failure cooldown.
		if ctx.Err() == nil {
			p.cache[target] = snapshot{err: err, expiresAt: p.clock.Now().Add(failureTTL)}
		}
		return nil, err
	}
	counts := map[string]int64{}
	for _, entry := range response.GetUtilizationDetails().GetIpv4Utilizations() {
		if entry.GetRangeName() != "" && entry.TotalFreeIp != nil {
			counts[entry.GetRangeName()] = *entry.TotalFreeIp
		}
	}
	p.cache[target] = snapshot{counts: counts, expiresAt: p.clock.Now().Add(cacheTTL)}
	return maps.Clone(counts), nil
}

// Invalidate discards all range counts for the affected subnetwork.
func (p *DefaultProvider) Invalidate(network, subnetwork string) {
	target, err := resolveTarget(network, subnetwork, p.region)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cache, target)
}

type subnetworkRef struct {
	project string
	region  string
	name    string
}

func (r subnetworkRef) String() string {
	return fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", r.project, r.region, r.name)
}

func resourceParts(raw string) ([]string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid resource reference %q", raw)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if slices.Contains(parts, "") {
		return nil, fmt.Errorf("invalid resource reference %q", raw)
	}
	if parsed.IsAbs() {
		index := slices.Index(parts, "projects")
		if index < 0 {
			return nil, fmt.Errorf("resource reference %q has no project", raw)
		}
		parts = parts[index:]
	}
	return parts, nil
}

func resolveTarget(network, subnetwork, region string) (subnetworkRef, error) {
	parts, err := resourceParts(subnetwork)
	if err != nil {
		return subnetworkRef{}, err
	}
	if len(parts) == 6 && parts[0] == "projects" && parts[2] == "regions" && parts[4] == "subnetworks" {
		return subnetworkRef{project: parts[1], region: parts[3], name: parts[5]}, nil
	}
	project, err := networkProject(network)
	if err != nil {
		return subnetworkRef{}, fmt.Errorf("resolving subnetwork %q: %w", subnetwork, err)
	}
	switch {
	case len(parts) == 1:
		return subnetworkRef{project: project, region: region, name: parts[0]}, nil
	case len(parts) == 4 && parts[0] == "regions" && parts[2] == "subnetworks":
		return subnetworkRef{project: project, region: parts[1], name: parts[3]}, nil
	default:
		return subnetworkRef{}, fmt.Errorf("invalid subnetwork reference %q", subnetwork)
	}
}

func networkProject(network string) (string, error) {
	parts, err := resourceParts(network)
	if err != nil {
		return "", err
	}
	if len(parts) != 5 || parts[0] != "projects" || parts[2] != "global" || parts[3] != "networks" {
		return "", fmt.Errorf("network %q is not fully qualified", network)
	}
	return parts[1], nil
}
