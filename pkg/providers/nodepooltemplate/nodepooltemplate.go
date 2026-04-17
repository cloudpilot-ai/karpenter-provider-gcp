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

package nodepooltemplate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

// Provider is the interface for managing the bootstrap source pool and its instance template.
type Provider interface {
	// Create selects the bootstrap source pool (or creates a last-resort fallback) and
	// caches the selected pool name. Called by the nodepooltemplate controller on a
	// 12-minute reconciliation cycle.
	Create(ctx context.Context) error
	// GetInstanceTemplates returns a single-entry map keyed by the selected source pool
	// name. Returns an error if no source pool has been discovered yet.
	GetInstanceTemplates(ctx context.Context) (map[string]*compute.InstanceTemplate, error)
	// GetSourcePoolName returns the name of the currently selected bootstrap source pool.
	GetSourcePoolName(ctx context.Context) (string, error)
}

type DefaultProvider struct {
	mu sync.RWMutex
	// sourcePoolName is the currently selected bootstrap source pool.
	sourcePoolName string

	computeService        *compute.Service
	containerService      *container.Service
	kubeClient            client.Client
	versionProvider       version.Provider
	ClusterInfo           ClusterInfo
	defaultServiceAccount string
	// preferredPoolName is the operator-pinned pool name (DEFAULT_NODEPOOL_TEMPLATE_NAME).
	// When non-empty, only this pool is eligible and an error is returned if it is not RUNNING.
	preferredPoolName string
}

type ClusterInfo struct {
	ProjectID       string
	ClusterLocation string
	NodeLocation    string
	Region          string
	Name            string
	Zones           []string
}

const (
	// KarpenterDefaultNodePoolTemplate is the last-resort fallback pool created when no
	// RUNNING cluster pool is available.
	KarpenterDefaultNodePoolTemplate          = "karpenter-default"
	KarpenterDefaultNodePoolTemplateImageType = "COS_CONTAINERD"

	// Legacy pool name constants — kept for migration logging only.
	// These pools are no longer created but may exist on clusters upgrading from older versions.
	KarpenterUbuntuNodePoolTemplate      = "karpenter-ubuntu"
	KarpenterCOSARM64NodePoolTemplate    = "karpenter-cos-arm64"
	KarpenterUbuntuARM64NodePoolTemplate = "karpenter-ubuntu-arm64"
)

// legacyKarpenterPools is the set of pool names that older Karpenter versions created.
// We log these at INFO to let operators know they can be cleaned up.
var legacyKarpenterPools = map[string]bool{
	KarpenterUbuntuNodePoolTemplate:      true,
	KarpenterCOSARM64NodePoolTemplate:    true,
	KarpenterUbuntuARM64NodePoolTemplate: true,
}

func NewDefaultProvider(ctx context.Context, kubeClient client.Client, computeService *compute.Service,
	containerService *container.Service, versionProvider version.Provider,
	clusterName, region, projectID, serviceAccount, clusterLocation, nodeLocation string,
	preferredPoolName string) *DefaultProvider {

	zones, err := resolveZones(ctx, computeService, projectID, region)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create default provider for node pool template")
		return nil
	}

	return &DefaultProvider{
		kubeClient:            kubeClient,
		computeService:        computeService,
		containerService:      containerService,
		versionProvider:       versionProvider,
		defaultServiceAccount: serviceAccount,
		preferredPoolName:     preferredPoolName,
		ClusterInfo: ClusterInfo{
			ProjectID:       projectID,
			ClusterLocation: clusterLocation,
			NodeLocation:    nodeLocation,
			Region:          region,
			Name:            clusterName,
			Zones:           zones,
		},
	}
}

func resolveZones(ctx context.Context, computeService *compute.Service, projectID, region string) ([]string, error) {
	logger := log.FromContext(ctx)
	logger.Info("resolving zones", "ProjectID", projectID, "Region", region)

	var zones []string
	prefix := region + "-"
	err := computeService.Zones.List(projectID).Pages(ctx, func(page *compute.ZoneList) error {
		for _, zone := range page.Items {
			if strings.HasPrefix(zone.Name, prefix) {
				zones = append(zones, zone.Name)
			}
		}
		return nil
	})
	if err != nil {
		logger.Error(err, "error listing zones from GCP")
		return nil, err
	}

	if len(zones) == 0 {
		logger.Info("no zones found matching region prefix", "region", region)
		return nil, fmt.Errorf("no zones found for region: %s", region)
	}

	logger.Info("resolved zones", "zones", zones)
	return zones, nil
}

// Create discovers the bootstrap source pool and caches its name. If no RUNNING pool
// is available, it creates a minimal last-resort fallback pool (karpenter-default) and
// returns an error so the controller requeues for a faster retry.
func (p *DefaultProvider) Create(ctx context.Context) error {
	logger := log.FromContext(ctx)

	selected, err := p.discoverSourcePool(ctx)
	if err != nil {
		logger.Error(err, "no RUNNING pool available; creating last-resort fallback pool",
			"fallback", KarpenterDefaultNodePoolTemplate)
		if createErr := p.ensureKarpenterNodePoolTemplate(ctx,
			KarpenterDefaultNodePoolTemplateImageType,
			KarpenterDefaultNodePoolTemplate,
			p.defaultServiceAccount); createErr != nil {
			return fmt.Errorf("creating fallback pool: %w", createErr)
		}
		// Return original error so the controller requeues; the pool may not be RUNNING
		// yet and will be picked up on the next reconcile.
		return fmt.Errorf("waiting for bootstrap pool to reach RUNNING: %w", err)
	}

	p.mu.Lock()
	prev := p.sourcePoolName
	p.sourcePoolName = selected
	p.mu.Unlock()

	if prev != selected {
		logger.Info("bootstrap source pool selected", "pool", selected, "previous", prev)
	}
	return nil
}

// isPoolEligible returns true for statuses that mean the pool's instance template is
// stable and can be used as a bootstrap source.
func isPoolEligible(status string) bool {
	return status == "RUNNING" || status == "RUNNING_WITH_ERROR"
}

// discoverSourcePool implements the pool selection algorithm from the proposal:
//  1. If DEFAULT_NODEPOOL_TEMPLATE_NAME is set → use that pool; error if not RUNNING.
//  2. If default-pool exists and is RUNNING → use it.
//  3. Sort remaining pools by name; use the first RUNNING pool.
//  4. No RUNNING pool → return error (caller creates fallback).
func (p *DefaultProvider) discoverSourcePool(ctx context.Context) (string, error) {
	if p.preferredPoolName != "" {
		return p.validatePreferredPool(ctx)
	}
	return p.selectFromClusterPools(ctx)
}

func (p *DefaultProvider) validatePreferredPool(ctx context.Context) (string, error) {
	pool, err := p.getNodePool(ctx, p.preferredPoolName)
	if err != nil {
		return "", fmt.Errorf("preferred pool %q not found: %w", p.preferredPoolName, err)
	}
	if !isPoolEligible(pool.Status) {
		return "", fmt.Errorf("preferred pool %q has status %q, not RUNNING", p.preferredPoolName, pool.Status)
	}
	return p.preferredPoolName, nil
}

func (p *DefaultProvider) selectFromClusterPools(ctx context.Context) (string, error) {
	logger := log.FromContext(ctx)

	clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.NodeLocation, p.ClusterInfo.Name)
	resp, err := p.containerService.Projects.Locations.Clusters.NodePools.
		List(clusterPath).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("listing node pools: %w", err)
	}

	p.logLegacyPools(logger, resp.NodePools)

	if name := firstEligibleNamedPool(resp.NodePools, "default-pool"); name != "" {
		return name, nil
	}

	candidates := eligiblePoolsSorted(resp.NodePools, "default-pool")
	if len(candidates) > 0 {
		return candidates[0], nil
	}

	return "", fmt.Errorf("no RUNNING node pool found in cluster %s/%s",
		p.ClusterInfo.NodeLocation, p.ClusterInfo.Name)
}

// logLegacyPools emits a single INFO line when legacy Karpenter-managed pools are found.
func (p *DefaultProvider) logLegacyPools(logger interface{ Info(string, ...interface{}) }, pools []*container.NodePool) {
	var legacy []string
	for _, pool := range pools {
		if legacyKarpenterPools[pool.Name] {
			legacy = append(legacy, pool.Name)
		}
	}
	if len(legacy) > 0 {
		logger.Info("legacy Karpenter-managed pools detected; they can be deleted manually",
			"pools", legacy)
	}
}

// firstEligibleNamedPool returns name if a pool with that name is eligible, else "".
func firstEligibleNamedPool(pools []*container.NodePool, name string) string {
	for _, pool := range pools {
		if pool.Name == name && isPoolEligible(pool.Status) {
			return name
		}
	}
	return ""
}

// eligiblePoolsSorted returns sorted names of all eligible pools, excluding excludeName.
func eligiblePoolsSorted(pools []*container.NodePool, excludeName string) []string {
	var candidates []string
	for _, pool := range pools {
		if isPoolEligible(pool.Status) && pool.Name != excludeName {
			candidates = append(candidates, pool.Name)
		}
	}
	sort.Strings(candidates)
	return candidates
}

// GetSourcePoolName returns the name of the currently selected bootstrap source pool.
// Returns an error if pool discovery has not completed yet.
func (p *DefaultProvider) GetSourcePoolName(_ context.Context) (string, error) {
	p.mu.RLock()
	name := p.sourcePoolName
	p.mu.RUnlock()
	if name == "" {
		return "", fmt.Errorf("bootstrap source pool not yet discovered")
	}
	return name, nil
}

// GetInstanceTemplates returns a single-entry map keyed by the selected source pool name.
func (p *DefaultProvider) GetInstanceTemplates(ctx context.Context) (map[string]*compute.InstanceTemplate, error) {
	poolName, err := p.GetSourcePoolName(ctx)
	if err != nil {
		return nil, err
	}

	template, err := p.getInstanceTemplate(ctx, poolName)
	if err != nil {
		return nil, err
	}
	if template == nil {
		return nil, fmt.Errorf("source pool %q is not RUNNING or has no instance template", poolName)
	}
	return map[string]*compute.InstanceTemplate{poolName: template}, nil
}

// ensureKarpenterNodePoolTemplate creates the fallback pool if it does not already exist.
// The pool config is intentionally minimal and compatible with common org policies:
// shielded VM options are enabled to satisfy compute.requireShieldedVm; CMEK options are
// left unset to avoid triggering gcp.restrictNonCmekServices.
func (p *DefaultProvider) ensureKarpenterNodePoolTemplate(ctx context.Context, imageType, nodePoolName, serviceAccount string) error {
	logger := log.FromContext(ctx)

	if p.ClusterInfo.Name == "" {
		return fmt.Errorf("clusterName is required but was empty")
	}

	logger.Info("ensuring fallback node pool exists",
		"projectID", p.ClusterInfo.ProjectID,
		"name", p.ClusterInfo.Name,
		"nodePoolName", nodePoolName)

	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.NodeLocation, p.ClusterInfo.Name, nodePoolName)

	_, err := p.containerService.Projects.Locations.Clusters.NodePools.Get(nodePoolSelfLink).Context(ctx).Do()
	if err == nil {
		logger.Info("fallback node pool already exists", "name", nodePoolName)
		return nil
	}

	var gcpErr *googleapi.Error
	if errors.As(err, &gcpErr) && gcpErr.Code != http.StatusNotFound {
		logger.Error(err, "failed to get node pool", "name", nodePoolName)
		return err
	}

	clusterSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.NodeLocation, p.ClusterInfo.Name)
	nodePoolOpts := &container.CreateNodePoolRequest{
		NodePool: &container.NodePool{
			Name:             nodePoolName,
			Autoscaling:      &container.NodePoolAutoscaling{Enabled: false},
			InitialNodeCount: 0,
			Config: &container.NodeConfig{
				ImageType:      imageType,
				ServiceAccount: serviceAccount,
				ShieldedInstanceConfig: &container.ShieldedInstanceConfig{
					EnableSecureBoot:          true,
					EnableIntegrityMonitoring: true,
				},
			},
		},
	}

	logger.Info("creating fallback node pool", "name", nodePoolName)
	_, err = p.containerService.Projects.Locations.Clusters.NodePools.
		Create(clusterSelfLink, nodePoolOpts).Context(ctx).Do()
	if err != nil {
		if errors.As(err, &gcpErr) && gcpErr.Code == http.StatusConflict {
			logger.Info("fallback node pool already created concurrently", "name", nodePoolName)
			return nil
		}
		logger.Error(err, "failed to create fallback node pool", "name", nodePoolName)
		return err
	}

	logger.Info("fallback node pool created successfully", "name", nodePoolName)
	return nil
}

func (p *DefaultProvider) getNodePool(ctx context.Context, nodePoolName string) (*container.NodePool, error) {
	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.NodeLocation, p.ClusterInfo.Name, nodePoolName)
	nodePool, err := p.containerService.Projects.Locations.Clusters.NodePools.
		Get(nodePoolSelfLink).Context(ctx).Do()
	if err != nil {
		log.FromContext(ctx).Error(err, "error getting node pool")
		return nil, err
	}
	return nodePool, nil
}

func (p *DefaultProvider) getInstanceTemplate(ctx context.Context, nodePoolName string) (*compute.InstanceTemplate, error) {
	logger := log.FromContext(ctx)

	if p.ClusterInfo.ProjectID == "" || p.ClusterInfo.Name == "" || p.ClusterInfo.Region == "" {
		return nil, fmt.Errorf("ClusterInfo not initialized")
	}

	nodePool, err := p.getNodePool(ctx, nodePoolName)
	if err != nil {
		return nil, err
	}

	if !isPoolEligible(nodePool.Status) {
		return nil, nil
	}

	if len(nodePool.InstanceGroupUrls) == 0 {
		return nil, fmt.Errorf("no instance group URLs found for node pool: %s", nodePoolName)
	}

	zone, managerName, err := resolveInstanceGroupZoneAndManagerName(nodePool.InstanceGroupUrls[0])
	if err != nil {
		logger.Error(err, "error resolving instance group URL")
		return nil, err
	}

	ig, err := p.computeService.InstanceGroupManagers.Get(p.ClusterInfo.ProjectID, zone, managerName).Do()
	if err != nil {
		logger.Error(err, "error getting instance group manager")
		return nil, err
	}

	templateName, err := resolveInstanceTemplateName(ig.InstanceTemplate)
	if err != nil {
		return nil, err
	}

	template, err := p.computeService.RegionInstanceTemplates.
		Get(p.ClusterInfo.ProjectID, p.ClusterInfo.Region, templateName).Context(ctx).Do()
	if err != nil {
		logger.Error(err, "error getting instance template")
		return nil, err
	}

	return template, nil
}

func resolveInstanceTemplateName(instanceTemplateURL string) (string, error) {
	parsedURL, err := url.Parse(instanceTemplateURL)
	if err != nil {
		return "", err
	}

	parts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")

	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "instanceTemplates" {
			return parts[i+1], nil
		}
	}

	return "", fmt.Errorf("invalid instance template URL: %s", instanceTemplateURL)
}

func resolveInstanceGroupZoneAndManagerName(instanceGroupURL string) (string, string, error) {
	parsedURL, err := url.Parse(instanceGroupURL)
	if err != nil {
		return "", "", err
	}

	parts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")

	if len(parts) < 8 || parts[4] != "zones" || parts[6] != "instanceGroupManagers" {
		return "", "", fmt.Errorf("invalid instance group URL: %s", instanceGroupURL)
	}

	zone := parts[5]
	instanceGroupManagerName := parts[7]

	return zone, instanceGroupManagerName, nil
}
