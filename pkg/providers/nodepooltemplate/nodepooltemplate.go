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
	"regexp"
	"strings"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
)

type Provider interface {
	Create(ctx context.Context) error
	GetInstanceTemplates(ctx context.Context) (map[string]*compute.InstanceTemplate, error)
}

type DefaultProvider struct {
	computeService   *compute.Service
	containerService *container.Service
	kubeClient       client.Client
	versionProvider  version.Provider
}

const (
	KarpenterDefaultNodePoolTemplate          = "karpenter-default"
	KarpenterDefaultNodePoolTemplateImageType = "COS_CONTAINERD"

	KarpenterUbuntuNodePoolTemplate          = "karpenter-ubuntu"
	KarpenterUbuntuNodePoolTemplateImageType = "UBUNTU_CONTAINERD"

	// ClusterNameNodeLabelKey is the key used to identify the cluster name
	// TODO: make sure the following key is correct
	ClusterNameNodeLabelKey = "goog-k8s-cluster-name"
)

func NewDefaultProvider(kubeClient client.Client, computeService *compute.Service,
	containerService *container.Service, versionProvider version.Provider) *DefaultProvider {
	return &DefaultProvider{
		kubeClient:       kubeClient,
		computeService:   computeService,
		containerService: containerService,
		versionProvider:  versionProvider,
	}
}

func (p *DefaultProvider) Create(ctx context.Context) error {
	projectID, region, clusterName, err := p.resolveClusterInfo(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "error resolving projectID and clusterName")
		return err
	}

	zones, err := p.resolveZones(ctx, projectID, region)
	if err != nil {
		return err
	}

	if err := p.ensureKarpenterNodePoolTemplate(ctx, projectID, region, clusterName, KarpenterDefaultNodePoolTemplateImageType, KarpenterDefaultNodePoolTemplate, zones); err != nil {
		return err
	}

	if err := p.ensureKarpenterNodePoolTemplate(ctx, projectID, region, clusterName, KarpenterUbuntuNodePoolTemplateImageType, KarpenterUbuntuNodePoolTemplate, zones); err != nil {
		return err
	}

	return nil
}

func (p *DefaultProvider) ensureKarpenterNodePoolTemplate(ctx context.Context,
	projectID, region, clusterName, imageType, nodePoolName string,
	zones []string) error {
	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		projectID, region, clusterName, nodePoolName)
	_, err := p.containerService.Projects.Locations.Clusters.NodePools.Get(nodePoolSelfLink).Context(ctx).Do()
	if err == nil {
		return nil
	}

	var gcpErr *googleapi.Error
	if errors.As(err, &gcpErr) && gcpErr.Code != http.StatusNotFound {
		log.FromContext(ctx).Error(err, "error getting node pool")
		return err
	}

	nodePoolOpts := &container.CreateNodePoolRequest{
		NodePool: &container.NodePool{
			Name:             nodePoolName,
			Autoscaling:      &container.NodePoolAutoscaling{Enabled: false},
			InitialNodeCount: 0,
			Locations:        []string{zones[0]},
			Config: &container.NodeConfig{
				ImageType: imageType,
			},
		},
	}
	clusterSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", projectID, region, clusterName)
	_, err = p.containerService.Projects.Locations.Clusters.NodePools.Create(clusterSelfLink, nodePoolOpts).Context(ctx).Do()
	if err != nil {
		log.FromContext(ctx).Error(err, "error creating node pool")
		return err
	}
	return nil
}

func (p *DefaultProvider) resolveZones(ctx context.Context, projectID, region string) ([]string, error) {
	var ret []string
	req := p.computeService.Zones.List(projectID)
	err := req.Pages(ctx, func(page *compute.ZoneList) error {
		for _, zone := range page.Items {
			if strings.HasPrefix(zone.Name, region) {
				ret = append(ret, zone.Name)
			}
		}
		return nil
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "error listing zones")
		return nil, err
	}

	if len(ret) == 0 {
		log.FromContext(ctx).Info("No zones found")
		return nil, fmt.Errorf("no zones found")
	}

	return ret, nil
}

func (p *DefaultProvider) resolveClusterInfo(ctx context.Context) (string, string, string, error) {
	var nodes v1.NodeList
	if err := p.kubeClient.List(ctx, &nodes); err != nil {
		log.FromContext(ctx).Error(err, "error listing nodes")
		return "", "", "", err
	}

	if len(nodes.Items) == 0 {
		return "", "", "", fmt.Errorf("no nodes found")
	}

	clusterName := nodes.Items[0].Labels[ClusterNameNodeLabelKey]
	providerID := nodes.Items[0].Spec.ProviderID
	re := regexp.MustCompile(`^gce://([^/]+)/([^/]+)/.*$`)
	matches := re.FindStringSubmatch(providerID)
	if len(matches) > 2 {
		projectID := matches[1]
		region := strings.Join(strings.Split(matches[2], "-")[:2], "-")
		return projectID, region, clusterName, nil
	}
	return "", clusterName, "", fmt.Errorf("failed to resolve projectID/region: %s", providerID)
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

func (p *DefaultProvider) getNodePool(ctx context.Context, projectID, region, clusterName, nodePoolName string) (*container.NodePool, error) {
	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		projectID, region, clusterName, nodePoolName)
	nodePool, err := p.containerService.Projects.Locations.Clusters.NodePools.Get(nodePoolSelfLink).Context(ctx).Do()
	if err != nil {
		log.FromContext(ctx).Error(err, "error getting node pool")
		return nil, err
	}
	return nodePool, nil
}

func (p *DefaultProvider) getInstanceTemplate(ctx context.Context, nodePoolName string) (*compute.InstanceTemplate, error) {
	projectID, region, clusterName, err := p.resolveClusterInfo(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "error resolving projectID and clusterName")
		return nil, err
	}

	nodePool, err := p.getNodePool(ctx, projectID, region, clusterName, nodePoolName)
	if err != nil {
		return nil, err
	}
	if nodePool.Status != "RUNNING" {
		return nil, nil
	}
	if len(nodePool.InstanceGroupUrls) != 0 {
		zone, managerName, err := resolveInstanceGroupZoneAndManagerName(nodePool.InstanceGroupUrls[0])
		if err != nil {
			log.FromContext(ctx).Error(err, "error resolving instance group url")
			return nil, err
		}

		ig, err := p.computeService.InstanceGroupManagers.Get(projectID, zone, managerName).Do()
		if err != nil {
			log.FromContext(ctx).Error(err, "error getting instance group manager")
			return nil, err
		}

		templateName, err := resolveInstanceTemplateName(ig.InstanceTemplate)
		if err != nil {
			return nil, err
		}

		template, err := p.computeService.RegionInstanceTemplates.Get(projectID, region, templateName).Context(ctx).Do()
		if err != nil {
			log.FromContext(ctx).Error(err, "error getting instance template")
			return nil, err
		}

		return template, nil
	}

	return nil, fmt.Errorf("instance template not found")
}

func resolveInstanceTemplateName(instanceTemplateURL string) (string, error) {
	// Parse the URL to extract path components
	parsedURL, err := url.Parse(instanceTemplateURL)
	if err != nil {
		return "", err
	}

	// Split the path into components
	parts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")

	// Ensure the path has enough components to extract project ID and instance template name
	if len(parts) < 8 || parts[6] != "instanceTemplates" {
		return "", fmt.Errorf("invalid instance template URL: %s", instanceTemplateURL)
	}

	// Extract project ID and instance template name
	instanceTemplateName := parts[7]

	return instanceTemplateName, nil
}

func resolveInstanceGroupZoneAndManagerName(instanceGroupURL string) (string, string, error) {
	// Parse the URL to extract path components
	parsedURL, err := url.Parse(instanceGroupURL)
	if err != nil {
		return "", "", err
	}

	// Split the path into components
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
