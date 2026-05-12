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
)

// Provider is the interface for managing the bootstrap source pool and its instance template.
type Provider interface {
	// Sync discovers the eligible bootstrap source pool and caches its name.
	// Returns an error if no eligible pool is found; the caller decides when to create a fallback.
	Sync(ctx context.Context) error
	// EnsureFallbackPool creates the karpenter-fallback pool if it does not already exist.
	EnsureFallbackPool(ctx context.Context) error
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
	// KarpenterFallbackNodePoolTemplate is the last-resort fallback pool created when no
	// RUNNING cluster pool is available.
	KarpenterFallbackNodePoolTemplate          = "karpenter-fallback"
	KarpenterFallbackNodePoolTemplateImageType = "COS_CONTAINERD"

)

func NewDefaultProvider(ctx context.Context, kubeClient client.Client, computeService *compute.Service,
	containerService *container.Service,
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

// Sync discovers the eligible bootstrap source pool and caches its name.
// Returns an error if no eligible pool is found; the caller decides when to create a fallback.
func (p *DefaultProvider) Sync(ctx context.Context) error {
	selected, err := p.discoverSourcePool(ctx)
	if err != nil {
		return err
	}

	p.mu.Lock()
	prev := p.sourcePoolName
	p.sourcePoolName = selected
	p.mu.Unlock()

	if prev != selected {
		log.FromContext(ctx).Info("bootstrap source pool selected", "pool", selected, "previous", prev)
	}
	return nil
}

// EnsureFallbackPool creates the karpenter-fallback pool if it does not already exist.
func (p *DefaultProvider) EnsureFallbackPool(ctx context.Context) error {
	return p.ensureKarpenterNodePoolTemplate(ctx, p.defaultServiceAccount)
}

// isPoolEligible returns true for statuses that mean the pool's instance template is
// stable and can be used as a bootstrap source.
func isPoolEligible(status string) bool {
	return status == "RUNNING" || status == "RUNNING_WITH_ERROR"
}

// discoverSourcePool implements the pool selection algorithm from the proposal:
//  1. If DEFAULT_NODEPOOL_TEMPLATE_NAME is set → use that pool; error if not eligible.
//  2. If default-pool exists and is eligible → use it.
//  3. Sort remaining pools by name; use the first eligible pool.
//  4. No eligible pool → return error (caller creates fallback).
//
// A pool is eligible when its status is RUNNING or RUNNING_WITH_ERROR.
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
		return "", fmt.Errorf("preferred pool %q has status %q (must be RUNNING or RUNNING_WITH_ERROR)", p.preferredPoolName, pool.Status)
	}
	return p.preferredPoolName, nil
}

func (p *DefaultProvider) selectFromClusterPools(ctx context.Context) (string, error) {
	clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.NodeLocation, p.ClusterInfo.Name)
	resp, err := p.containerService.Projects.Locations.Clusters.NodePools.
		List(clusterPath).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("listing node pools: %w", err)
	}

	if name := firstEligibleNamedPool(resp.NodePools, "default-pool"); name != "" {
		return name, nil
	}

	candidates := eligiblePoolsSorted(resp.NodePools, "default-pool")
	if len(candidates) > 0 {
		return candidates[0], nil
	}

	return "", fmt.Errorf("no eligible node pool found in cluster %s/%s (pools must be RUNNING or RUNNING_WITH_ERROR)",
		p.ClusterInfo.NodeLocation, p.ClusterInfo.Name)
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
		return nil, fmt.Errorf("source pool %q has no instance template", poolName)
	}
	return map[string]*compute.InstanceTemplate{poolName: template}, nil
}

// ensureKarpenterNodePoolTemplate creates the fallback pool if it does not already exist.
// The pool config is hardened against known GCP org policy constraints:
//   - compute.requireShieldedVm / container.managed.enableShieldedNodes: Shielded VM always on.
//   - container.managed.enablePrivateNodes: mirrored from cluster config.
//   - container.managed.disableInsecureKubeletReadOnlyPort: always disabled.
//   - container.managed.enableWorkloadIdentityFederation: GKE_METADATA mode set when WI is active.
//   - compute.managed.blockProjectSshKeys: always set in node metadata.
//
// gcp.restrictNonCmekServices cannot be auto-satisfied (requires a pre-existing customer-managed
// KMS key). Fallback creation will fail on such clusters; the operator must pre-create a RUNNING
// pool and set DEFAULT_NODEPOOL_TEMPLATE_NAME.
func (p *DefaultProvider) ensureKarpenterNodePoolTemplate(ctx context.Context, serviceAccount string) error {
	logger := log.FromContext(ctx)
	nodePoolName := KarpenterFallbackNodePoolTemplate

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

	// Fetch cluster config to apply policy-aware settings. Non-fatal: if unavailable,
	// conditional fields (private nodes, Workload Identity) are omitted and safe defaults apply.
	cluster, clusterErr := p.fetchClusterConfig(ctx)
	if clusterErr != nil {
		logger.Error(clusterErr, "failed to fetch cluster config; fallback pool will use minimal defaults")
	}

	nodePool := buildFallbackNodePool(cluster, nodePoolName, serviceAccount)

	clusterSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.NodeLocation, p.ClusterInfo.Name)

	logger.Info("creating fallback node pool", "name", nodePoolName)
	_, err = p.containerService.Projects.Locations.Clusters.NodePools.
		Create(clusterSelfLink, &container.CreateNodePoolRequest{NodePool: nodePool}).Context(ctx).Do()
	if err != nil {
		if errors.As(err, &gcpErr) && gcpErr.Code == http.StatusConflict {
			logger.Info("fallback node pool already created concurrently", "name", nodePoolName)
			return nil
		}
		logger.Error(err, "failed to create fallback node pool", "name", nodePoolName, "serviceAccount", serviceAccount)
		return fmt.Errorf("creating fallback pool (serviceAccount=%s): %w", serviceAccount, err)
	}

	logger.Info("fallback node pool created successfully", "name", nodePoolName)
	return nil
}

// fetchClusterConfig retrieves the cluster object from the GKE API to read cluster-level
// policy settings (private nodes, Workload Identity) for fallback pool configuration.
func (p *DefaultProvider) fetchClusterConfig(ctx context.Context) (*container.Cluster, error) {
	selfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.NodeLocation, p.ClusterInfo.Name)
	return p.containerService.Projects.Locations.Clusters.Get(selfLink).Context(ctx).Do()
}

// buildFallbackNodePool constructs the NodePool definition for the last-resort fallback pool,
// applying cluster-aware policy settings when cluster config is available.
func buildFallbackNodePool(cluster *container.Cluster, poolName, serviceAccount string) *container.NodePool {
	nodeConfig := &container.NodeConfig{
		ImageType:      KarpenterFallbackNodePoolTemplateImageType,
		ServiceAccount: serviceAccount,
		ShieldedInstanceConfig: &container.ShieldedInstanceConfig{
			EnableSecureBoot:          true,
			EnableIntegrityMonitoring: true,
		},
		KubeletConfig: &container.NodeKubeletConfig{
			InsecureKubeletReadonlyPortEnabled: false,
		},
		Metadata: map[string]string{
			"block-project-ssh-keys": "true",
		},
	}
	if clusterWorkloadPool(cluster) != "" {
		nodeConfig.WorkloadMetadataConfig = &container.WorkloadMetadataConfig{Mode: "GKE_METADATA"}
	}
	nodePool := &container.NodePool{
		Name:             poolName,
		Autoscaling:      &container.NodePoolAutoscaling{Enabled: false},
		InitialNodeCount: 0,
		Config:           nodeConfig,
	}
	if clusterHasPrivateNodes(cluster) {
		nodePool.NetworkConfig = &container.NodeNetworkConfig{EnablePrivateNodes: true}
	}
	return nodePool
}

// clusterHasPrivateNodes reports whether the cluster requires private nodes.
func clusterHasPrivateNodes(cluster *container.Cluster) bool {
	if cluster == nil {
		return false
	}
	return (cluster.NetworkConfig != nil && cluster.NetworkConfig.DefaultEnablePrivateNodes) ||
		(cluster.PrivateClusterConfig != nil && cluster.PrivateClusterConfig.EnablePrivateNodes)
}

// clusterWorkloadPool returns the cluster's Workload Identity pool name, or "" if not configured.
func clusterWorkloadPool(cluster *container.Cluster) string {
	if cluster == nil || cluster.WorkloadIdentityConfig == nil {
		return ""
	}
	return cluster.WorkloadIdentityConfig.WorkloadPool
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

	// Look for the last part only if path contains "instanceTemplates"
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

	// Ensure the path has enough components to extract zone and instance group manager name
	if len(parts) < 8 || parts[4] != "zones" || parts[6] != "instanceGroupManagers" {
		return "", "", fmt.Errorf("invalid instance group URL: %s", instanceGroupURL)
	}

	// Extract zone and instance group manager name
	zone := parts[5]
	instanceGroupManagerName := parts[7]

	return zone, instanceGroupManagerName, nil
}
