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
	"time"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

type Provider interface {
	Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodePool *v1.NodePool) error
	Delete(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) error
	GetInstanceTemplate(ctx context.Context, nodePoolName string) (*compute.InstanceTemplate, error)
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
	Region    string
	Name      string
	Zones     []string
}

const (
	KarpenterDefaultNodePoolTemplateImageType = "COS_CONTAINERD"

	KarpenterUbuntuNodePoolTemplateImageType = "UBUNTU_CONTAINERD"
)

func NewDefaultProvider(ctx context.Context, kubeClient client.Client, computeService *compute.Service,
	containerService *container.Service, versionProvider version.Provider,
	clusterName, region, projectID, serviceAccount string) *DefaultProvider {
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
			Region:    region,
			Name:      strings.TrimSuffix(clusterName, "."),
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

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodePool *v1.NodePool) error {
	nodePoolName := utils.ResolveNodePoolName(nodeClass.Name)
	imageType, err := p.resolveImageType(nodeClass.ImageFamily())
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve image type", "nodeClass", nodeClass.Name)
		return err
	}

	var maxPods int64 = v1alpha1.KubeletMaxPods
	if nodeClass.Spec.KubeletConfiguration != nil && nodeClass.Spec.KubeletConfiguration.MaxPods != nil {
		maxPods = int64(*nodeClass.Spec.KubeletConfiguration.MaxPods)
	}

	if err := p.ensureNodePoolTemplate(ctx, imageType, nodePoolName, p.defaultServiceAccount, maxPods, nodePool); err != nil {
		log.FromContext(ctx).Error(err, "failed to ensure node pool template", "nodeClass", nodeClass.Name)
		return err
	}
	return nil
}

func (p *DefaultProvider) Delete(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) error {
	nodePoolName := utils.ResolveNodePoolName(nodeClass.Name)
	logger := log.FromContext(ctx)
	logger.Info("deleting node pool template", "name", nodePoolName)

	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.Region, p.ClusterInfo.Name, nodePoolName)

	op, err := p.containerService.Projects.Locations.Clusters.NodePools.Delete(nodePoolSelfLink).Context(ctx).Do()
	if err != nil {
		var gcpErr *googleapi.Error
		if errors.As(err, &gcpErr) {
			if gcpErr.Code == http.StatusNotFound {
				logger.Info("node pool template already deleted", "name", nodePoolName)
				return nil
			}
			if gcpErr.Code == http.StatusBadRequest && strings.Contains(gcpErr.Message, "CLUSTER_ALREADY_HAS_OPERATION") {
				logger.Info("cluster has a conflicting operation in progress, requeueing delete", "name", nodePoolName)
				return fmt.Errorf("cluster has a conflicting operation in progress")
			}
		}
		logger.Error(err, "failed to delete node pool template", "name", nodePoolName)
		return err
	}
	if err := p.waitForOperation(ctx, op); err != nil {
		return err
	}
	logger.Info("node pool template deleted", "name", nodePoolName)
	return nil
}

func (p *DefaultProvider) resolveImageType(imageFamily string) (string, error) {
	switch imageFamily {
	case v1alpha1.ImageFamilyContainerOptimizedOS:
		return KarpenterDefaultNodePoolTemplateImageType, nil
	case v1alpha1.ImageFamilyUbuntu:
		return KarpenterUbuntuNodePoolTemplateImageType, nil
	default:
		return "", fmt.Errorf("unsupported image family %q", imageFamily)
	}
}

func (p *DefaultProvider) ensureNodePoolTemplate(ctx context.Context, imageType, nodePoolName, serviceAccount string, maxPods int64, nodePool *v1.NodePool) error {
	logger := log.FromContext(ctx)
	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.Region, p.ClusterInfo.Name, nodePoolName)

	existing, err := p.containerService.Projects.Locations.Clusters.NodePools.Get(nodePoolSelfLink).Context(ctx).Do()
	if err != nil {
		var gcpErr *googleapi.Error
		if errors.As(err, &gcpErr) && gcpErr.Code == http.StatusNotFound {
			return p.createNodePoolTemplate(ctx, imageType, nodePoolName, serviceAccount, maxPods, nodePool)
		}
		logger.Error(err, "failed to get node pool", "name", nodePoolName)
		return err
	}

	// Node pool exists, check if it needs an update.
	if existing.MaxPodsConstraint.MaxPodsPerNode != maxPods ||
		existing.Config.Labels == nil ||
		existing.Config.Labels[v1alpha1.GKENodePoolLabel] != nodePoolName {
		logger.Info("node pool template is outdated, recreating", "name", nodePoolName)
		if err := p.Delete(ctx, &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: strings.TrimPrefix(nodePoolName, "karpenter-")}}); err != nil {
			return err
		}
		return p.createNodePoolTemplate(ctx, imageType, nodePoolName, serviceAccount, maxPods, nodePool)
	}

	return nil
}

func (p *DefaultProvider) createNodePoolTemplate(ctx context.Context, imageType, nodePoolName, serviceAccount string, maxPods int64, nodePool *v1.NodePool) error {
	logger := log.FromContext(ctx)
	logger.Info("creating node pool template", "name", nodePoolName)

	clusterSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", p.ClusterInfo.ProjectID, p.ClusterInfo.Region, p.ClusterInfo.Name)
	cluster, err := p.containerService.Projects.Locations.Clusters.Get(clusterSelfLink).Context(ctx).Do()
	if err != nil {
		logger.Error(err, "failed to get cluster info for nodepool creation")
		return err
	}

	var nodepoolZones *v1.NodeSelectorRequirementWithMinValues
	for i := range nodePool.Spec.Template.Spec.Requirements {
		if nodePool.Spec.Template.Spec.Requirements[i].Key == "topology.kubernetes.io/zone" {
			nodepoolZones = &nodePool.Spec.Template.Spec.Requirements[i]
			break
		}
	}

	if nodepoolZones == nil {
		return fmt.Errorf("no zones specified in nodepool %s", nodePool.Name)
	}
	zones := lo.Intersect(p.ClusterInfo.Zones, nodepoolZones.Values)

	nodePoolOpts := &container.CreateNodePoolRequest{
		NodePool: &container.NodePool{
			Name: nodePoolName,
			Autoscaling: &container.NodePoolAutoscaling{
				Enabled: false,
			},
			InitialNodeCount: 0,
			Locations:        zones,
			Config: &container.NodeConfig{
				ImageType:      imageType,
				ServiceAccount: serviceAccount,
				Labels: map[string]string{
					v1alpha1.GKENodePoolLabel: nodePoolName,
				},
			},
			MaxPodsConstraint: &container.MaxPodsConstraint{
				MaxPodsPerNode: maxPods,
			},
			NetworkConfig: &container.NodeNetworkConfig{
				PodRange: cluster.IpAllocationPolicy.ClusterSecondaryRangeName,
			},
		},
	}

	op, err := p.containerService.Projects.Locations.Clusters.NodePools.Create(clusterSelfLink, nodePoolOpts).Context(ctx).Do()
	if err != nil {
		var gcpErr *googleapi.Error
		if errors.As(err, &gcpErr) {
			if gcpErr.Code == http.StatusConflict {
				logger.Info("node pool template already created concurrently", "name", nodePoolName)
				return nil
			}
			if gcpErr.Code == http.StatusBadRequest && strings.Contains(gcpErr.Message, "CLUSTER_ALREADY_HAS_OPERATION") {
				logger.Info("cluster has a conflicting operation in progress, requeueing", "name", nodePoolName)
				return fmt.Errorf("cluster has a conflicting operation in progress")
			}
		}
		logger.Error(err, "failed to create node pool template", "name", nodePoolName)
		return err
	}
	if err := p.waitForOperation(ctx, op); err != nil {
		return err
	}
	logger.Info("node pool template created", "name", nodePoolName)
	return nil
}

func (p *DefaultProvider) waitForOperation(ctx context.Context, op *container.Operation) error {
	logger := log.FromContext(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			var err error
			u, err := url.Parse(op.SelfLink)
			if err != nil {
				return fmt.Errorf("parsing operation selfLink %q, %w", op.SelfLink, err)
			}
			opName := strings.TrimPrefix(u.Path, "/v1/")
			op, err = p.containerService.Projects.Locations.Operations.Get(opName).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("polling operation status, %w", err)
			}
			if op.Status == "DONE" {
				if op.Error != nil {
					return fmt.Errorf("operation failed: %v", op.Error)
				}
				logger.Info("gke operation finished", "operation", op.Name, "type", op.OperationType)
				return nil
			}
			logger.Info("waiting for gke operation", "operation", op.Name, "type", op.OperationType, "status", op.Status)
		}
	}
}

func (p *DefaultProvider) GetInstanceTemplate(ctx context.Context, nodePoolName string) (*compute.InstanceTemplate, error) {
	defaultTemplate, err := p.getInstanceTemplate(ctx, nodePoolName)
	if err != nil {
		return nil, fmt.Errorf("getting instance template for node pool %s: %w", nodePoolName, err)
	}
	return defaultTemplate, nil
}

func (p *DefaultProvider) getNodePool(ctx context.Context, nodePoolName string) (*container.NodePool, error) {
	nodePoolSelfLink := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
		p.ClusterInfo.ProjectID, p.ClusterInfo.Region, p.ClusterInfo.Name, nodePoolName)
	nodePool, err := p.containerService.Projects.Locations.Clusters.NodePools.Get(nodePoolSelfLink).Context(ctx).Do()
	if err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
			log.FromContext(ctx).Info("node pool not found", "name", nodePoolName)
			return nil, nil // Not found is not an error
		}
		log.FromContext(ctx).Error(err, "error getting node pool")
		return nil, err
	}
	return nodePool, nil
}

func (p *DefaultProvider) getInstanceTemplate(ctx context.Context, nodePoolName string) (*compute.InstanceTemplate, error) {
	logger := log.FromContext(ctx)

	if err := p.validateClusterInfo(); err != nil {
		return nil, err
	}

	nodePool, err := p.getNodePool(ctx, nodePoolName)
	if err != nil || nodePool == nil || nodePool.Status != "RUNNING" {
		return nil, err
	}

	if len(nodePool.InstanceGroupUrls) == 0 {
		return nil, fmt.Errorf("no instance group URLs found for node pool: %s", nodePoolName)
	}

	location, managerName, isRegional, err := resolveInstanceGroupZoneAndManagerName(nodePool.InstanceGroupUrls[0])
	if err != nil {
		logger.Error(err, "error resolving instance group URL")
		return nil, err
	}

	ig, err := p.getInstanceGroupManager(location, managerName, isRegional)
	if err != nil {
		return nil, err
	}

	templateName, err := resolveInstanceTemplateName(ig.InstanceTemplate)
	if err != nil {
		return nil, err
	}

	return p.getRegionalInstanceTemplate(ctx, templateName)
}

func (p *DefaultProvider) validateClusterInfo() error {
	if p.ClusterInfo.ProjectID == "" || p.ClusterInfo.Name == "" || p.ClusterInfo.Region == "" {
		return fmt.Errorf("ClusterInfo not initialized")
	}
	return nil
}

func (p *DefaultProvider) getInstanceGroupManager(location, managerName string, isRegional bool) (*compute.InstanceGroupManager, error) {
	if isRegional {
		ig, err := p.computeService.RegionInstanceGroupManagers.Get(p.ClusterInfo.ProjectID, location, managerName).Do()
		if err != nil {
			return nil, fmt.Errorf("getting instance group manager: %w", err)
		}
		return ig, nil
	}
	ig, err := p.computeService.InstanceGroupManagers.Get(p.ClusterInfo.ProjectID, location, managerName).Do()
	if err != nil {
		return nil, fmt.Errorf("getting instance group manager: %w", err)
	}
	return ig, nil
}

func (p *DefaultProvider) getRegionalInstanceTemplate(ctx context.Context, templateName string) (*compute.InstanceTemplate, error) {
	template, err := p.computeService.RegionInstanceTemplates.Get(p.ClusterInfo.ProjectID, p.ClusterInfo.Region, templateName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("getting instance template: %w", err)
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

func resolveInstanceGroupZoneAndManagerName(instanceGroupURL string) (string, string, bool, error) {
	parsedURL, err := url.Parse(instanceGroupURL)
	if err != nil {
		return "", "", false, err
	}

	parts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	for i, p := range parts {
		if p == "instanceGroupManagers" && i > 1 && i+1 < len(parts) {
			locationType := parts[i-2]
			if locationType == "zones" || locationType == "regions" {
				return parts[i-1], parts[i+1], locationType == "regions", nil
			}
		}
	}
	return "", "", false, fmt.Errorf("invalid instance group URL: %s", instanceGroupURL)
}
