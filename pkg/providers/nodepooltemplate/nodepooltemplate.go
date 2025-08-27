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
	"os"
	"strings"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

type Provider interface {
	Create(ctx context.Context) error
	GetInstanceTemplates(ctx context.Context) (map[string]*compute.InstanceTemplate, error)
}

type DefaultProvider struct {
	computeService        *compute.Service
	containerService      *container.Service
	kubeClient            client.Client
	versionProvider       version.Provider
	ClusterInfo           ClusterInfo
	defaultServiceAccount string
}

type ClusterInfo struct {
	ProjectID string
	Location  string
	Region    string
	Name      string
	Zones     []string
}

const (
	KarpenterDefaultNodePoolTemplate          = "karpenter-default"
	KarpenterDefaultNodePoolTemplateImageType = "COS_CONTAINERD"

	KarpenterUbuntuNodePoolTemplate          = "karpenter-ubuntu"
	KarpenterUbuntuNodePoolTemplateImageType = "UBUNTU_CONTAINERD"
)

func NewDefaultProvider(ctx context.Context, kubeClient client.Client, computeService *compute.Service,
	containerService *container.Service, versionProvider version.Provider,
	clusterName, region, projectID, serviceAccount, location string) *DefaultProvider {

	zones, err := resolveZones(ctx, computeService, projectID, region)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create default provider for node pool template")
		os.Exit(1)
	}

	return &DefaultProvider{
		kubeClient:            kubeClient,
		computeService:        computeService,
		containerService:      containerService,
		versionProvider:       versionProvider,
		defaultServiceAccount: serviceAccount,
		ClusterInfo: ClusterInfo{
			ProjectID: projectID,
			Location:  location,
			Region:    region,
			Name:      clusterName,
			Zones:     zones,
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

// creating both default nodepool templates could be run concurrently
func (p *DefaultProvider) Create(ctx context.Context) error {
	if err := p.ensureKarpenterNodePoolTemplate(ctx, KarpenterDefaultNodePoolTemplateImageType, KarpenterDefaultNodePoolTemplate, p.defaultServiceAccount); err != nil {
		return err
	}

	if err := p.ensureKarpenterNodePoolTemplate(ctx, KarpenterUbuntuNodePoolTemplateImageType, KarpenterUbuntuNodePoolTemplate, p.defaultServiceAccount); err != nil {
		return err
	}

	return nil
}

func (p *DefaultProvider) ensureKarpenterNodePoolTemplate(ctx context.Context, imageType, nodePoolName, serviceAccount string) error {
	logger := log.FromContext(ctx)

	// adding simple validation, because previous code was failing
	// here and no reasonable log was printed out
	if p.ClusterInfo.Name == "" {
		return fmt.Errorf("clusterName is required but was empty")
	}
	if len(p.ClusterInfo.Zones) == 0 {
		return fmt.Errorf("no zones provided for node pool %s", nodePoolName)
	}

	logger.Info("ensuring node pool template exists",
		"projectID", p.ClusterInfo.ProjectID,
		"region", p.ClusterInfo.Region,
		"name", p.ClusterInfo.Name,
		"nodePoolName", nodePoolName,
		"zones", p.ClusterInfo.Zones)

	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.Location, p.ClusterInfo.Name, nodePoolName)

	_, err := p.containerService.Projects.Locations.Clusters.NodePools.Get(nodePoolSelfLink).Context(ctx).Do()
	if err == nil {
		logger.Info("template node pool already exists", "name", nodePoolName)
		return nil
	}

	var gcpErr *googleapi.Error
	if errors.As(err, &gcpErr) && gcpErr.Code != http.StatusNotFound {
		logger.Error(err, "Failed to get node pool", "name", nodePoolName, "nodePoolSelfLink", nodePoolSelfLink)
		return err
	}

	// Prepare request
	nodePoolOpts := &container.CreateNodePoolRequest{
		NodePool: &container.NodePool{
			Name:             nodePoolName,
			Autoscaling:      &container.NodePoolAutoscaling{Enabled: false},
			InitialNodeCount: 0,
			Locations:        p.ClusterInfo.Zones,
			Config: &container.NodeConfig{
				ImageType:      imageType,
				ServiceAccount: serviceAccount,
			},
		},
	}

	logger.Info("creating node pool", "name", nodePoolName)
	clusterSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", p.ClusterInfo.ProjectID, p.ClusterInfo.Location, p.ClusterInfo.Name)
	_, err = p.containerService.Projects.Locations.Clusters.NodePools.Create(clusterSelfLink, nodePoolOpts).Context(ctx).Do()
	if err != nil {
		if errors.As(err, &gcpErr) && gcpErr.Code == http.StatusConflict {
			logger.Info("node pool already created concurrently", "name", nodePoolName)
			return nil
		}
		logger.Error(err, "failed to create node pool", "name", nodePoolName)
		return err
	}

	logger.Info("node pool created successfully", "name", nodePoolName)
	return nil
}

func (p *DefaultProvider) GetInstanceTemplates(ctx context.Context) (map[string]*compute.InstanceTemplate, error) {
	ret := map[string]*compute.InstanceTemplate{}
	defaultTemplate, err := p.getInstanceTemplate(ctx, KarpenterDefaultNodePoolTemplate)
	if err != nil {
		return nil, err
	}
	if defaultTemplate != nil {
		ret[KarpenterDefaultNodePoolTemplate] = defaultTemplate
	}

	ubuntuTemplate, err := p.getInstanceTemplate(ctx, KarpenterUbuntuNodePoolTemplate)
	if err != nil {
		return nil, err
	}
	if ubuntuTemplate != nil {
		ret[KarpenterUbuntuNodePoolTemplate] = ubuntuTemplate
	}

	return ret, nil
}

func (p *DefaultProvider) getNodePool(ctx context.Context, nodePoolName string) (*container.NodePool, error) {
	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.Location, p.ClusterInfo.Name, nodePoolName)
	nodePool, err := p.containerService.Projects.Locations.Clusters.NodePools.Get(nodePoolSelfLink).Context(ctx).Do()
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

	if nodePool.Status != "RUNNING" {
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

	template, err := p.computeService.RegionInstanceTemplates.Get(p.ClusterInfo.ProjectID, p.ClusterInfo.Region, templateName).Context(ctx).Do()
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
