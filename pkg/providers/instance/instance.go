/*
Copyright 2024 The CloudPilot AI Authors.

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

package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

type Provider interface {
	Create(context.Context, *v1alpha1.GCENodeClass, *karpv1.NodeClaim, []*cloudprovider.InstanceType) (*Instance, error)
	Get(context.Context, string) (*Instance, error)
	List(context.Context) ([]*Instance, error)
	Delete(context.Context, string) error
	CreateTags(context.Context, string, map[string]string) error
}

type DefaultProvider struct {
	region         string
	projectID      string
	computeService *compute.Service
}

func NewProvider(region, projectID string, computeService *compute.Service) *DefaultProvider {
	return &DefaultProvider{
		region:         region,
		projectID:      projectID,
		computeService: computeService,
	}
}

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error) {
	if len(instanceTypes) == 0 {
		return nil, fmt.Errorf("no instance types provided")
	}

	// all code below must be adjusted with correct logic!
	// lets always create e2-standard-2 in us-central1-c at least for now
	instanceType, err := p.selectInstanceType(instanceTypes)
	if err != nil {
		return nil, err
	}

	zone, err := p.selectZone(nodeClaim)
	if err != nil {
		return nil, err
	}

	template, err := p.findTemplateForAlias(ctx, nodeClass.Spec.ImageSelectorTerms[0].Alias)
	if err != nil {
		return nil, err
	}

	instance := p.buildInstance(nodeClaim, nodeClass, instanceType, template, zone)

	op, err := p.computeService.Instances.Insert(p.projectID, zone, instance).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("creating instance: %w", err)
	}

	// we could wait for the node to be present in kubernetes api via csr sign up
	// should be done with watcher, for now implemented as a csr controller
	log.FromContext(ctx).Info("Created instance", "instanceName", op.Name)

	return &Instance{
		InstanceID:   instance.Name,
		Name:         instance.Name,
		Type:         instanceType.Name,
		Location:     zone,
		ProjectID:    p.projectID,
		ImageID:      template.Properties.Disks[0].InitializeParams.SourceImage,
		CreationTime: time.Now(),
		CapacityType: karpv1.CapacityTypeOnDemand,
		Tags:         template.Properties.Labels,
		Labels:       instance.Labels,
		Status:       InstanceStatusProvisioning,
	}, nil
}

// lets always create e2-standard-2 for now
func (p *DefaultProvider) selectInstanceType(instanceTypes []*cloudprovider.InstanceType) (*cloudprovider.InstanceType, error) {
	for _, it := range instanceTypes {
		if it.Name == "e2-standard-2" {
			return it, nil
		}
	}
	return nil, fmt.Errorf("instance type e2-standard-2 not found")
}

// zone should be based on the offering, for now lets return static zone from requirements
func (p *DefaultProvider) selectZone(nodeClaim *karpv1.NodeClaim) (string, error) {
	for _, req := range nodeClaim.Spec.Requirements {
		if req.Key == "topology.kubernetes.io/zone" && len(req.Values) > 0 {
			return req.Values[0], nil // always select us-central1-c for now
		}
	}
	return "", fmt.Errorf("no zone specified in nodeClaim requirements")
}

func (p *DefaultProvider) findTemplateForAlias(ctx context.Context, alias string) (*compute.InstanceTemplate, error) {
	if alias == "" {
		return nil, fmt.Errorf("alias not specified in ImageSelectorTerm")
	}

	var expectedLabelValue string
	switch alias {
	case "ContainerOptimizedOS":
		expectedLabelValue = nodepooltemplate.KarpenterDefaultNodePoolTemplate
	case "Ubuntu":
		expectedLabelValue = nodepooltemplate.KarpenterUbuntuNodePoolTemplate
	default:
		return nil, fmt.Errorf("unsupported image alias %q", alias)
	}

	instanceTemplates, err := p.computeService.RegionInstanceTemplates.List(p.projectID, p.region).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("cannot list all instance templates for alias %q: %w", alias, err)
	}

	for _, t := range instanceTemplates.Items {
		if t.Properties != nil && t.Properties.Labels != nil {
			if val, ok := t.Properties.Labels["goog-k8s-node-pool-name"]; ok && val == expectedLabelValue {
				return t, nil
			}
		}
	}

	return nil, fmt.Errorf("no instance template found with label goog-k8s-node-pool-name=%s for alias %q", expectedLabelValue, alias)
}

func (p *DefaultProvider) buildInstance(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, template *compute.InstanceTemplate, zone string) *compute.Instance {
	disk := template.Properties.Disks[0]
	disk.InitializeParams.DiskType = fmt.Sprintf("projects/%s/zones/%s/diskTypes/pd-balanced", p.projectID, zone)

	// disable public IPs
	for _, ni := range template.Properties.NetworkInterfaces {
		ni.AccessConfigs = nil
	}

	instance := &compute.Instance{
		Name:              fmt.Sprintf("karpenter-%s", nodeClaim.Name),
		MachineType:       fmt.Sprintf("zones/%s/machineTypes/%s", zone, instanceType.Name),
		Disks:             []*compute.AttachedDisk{disk},
		NetworkInterfaces: template.Properties.NetworkInterfaces,
		ServiceAccounts:   template.Properties.ServiceAccounts,
		Metadata:          template.Properties.Metadata,
		Labels:            map[string]string{},
		Scheduling:        template.Properties.Scheduling,
		Tags:              template.Properties.Tags,
	}

	// apply scheduling config for Spot capacity, for now lets do on demand only
	capacityType := karpv1.CapacityTypeOnDemand
	if instance.Scheduling == nil {
		instance.Scheduling = &compute.Scheduling{}
	}
	if capacityType == karpv1.CapacityTypeSpot {
		instance.Scheduling.ProvisioningModel = "SPOT"
		instance.Scheduling.Preemptible = true
		instance.Scheduling.AutomaticRestart = ptr.To(false)
		instance.Scheduling.OnHostMaintenance = "TERMINATE"
	}

	// set common Karpenter labels
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelNodePoolKey)] = nodeClaim.Labels[karpv1.NodePoolLabelKey]
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey)] = nodeClass.Name

	return instance
}

func (p *DefaultProvider) Get(ctx context.Context, providerID string) (*Instance, error) {
	project, zone, instanceName, err := parseGCEProviderID(providerID)
	if err != nil {
		return nil, fmt.Errorf("parsing provider ID: %w", err)
	}

	log := log.FromContext(ctx)
	log.Info("Fetching instance", "project", project, "zone", zone, "instance", instanceName)

	resp, err := p.computeService.Instances.Get(project, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance not found: %w", err))
		}
		return nil, fmt.Errorf("getting instance: %w", err)
	}

	// Translate Compute API response into internal Instance struct
	instance := &Instance{
		InstanceID:   resp.Name,
		Name:         resp.Name,
		Type:         resp.MachineType[strings.LastIndex(resp.MachineType, "/")+1:], // extract type from full URI
		Location:     zone,
		ProjectID:    project,
		ImageID:      getBootImageID(resp),
		CreationTime: parseCreationTime(resp.CreationTimestamp),
		CapacityType: resolveCapacityType(resp.Scheduling),
		Labels:       resp.Labels,
		Tags:         resp.Labels,           // GCP doesn't have separate tags like AWS; labels suffice
		Status:       InstanceStatusRunning, // consider deriving from resp.Status if needed
	}

	return instance, nil
}

func parseCreationTime(ts string) time.Time {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Now() // fallback
	}
	return parsed
}

func getBootImageID(inst *compute.Instance) string {
	if len(inst.Disks) > 0 && inst.Disks[0].InitializeParams != nil {
		return inst.Disks[0].InitializeParams.SourceImage
	}
	return ""
}

func resolveCapacityType(sched *compute.Scheduling) string {
	if sched != nil && sched.ProvisioningModel == "SPOT" {
		return karpv1.CapacityTypeSpot
	}
	return karpv1.CapacityTypeOnDemand
}

func (p *DefaultProvider) List(ctx context.Context) ([]*Instance, error) {
	var instances []*Instance
	filter := fmt.Sprintf(
		"(labels.%s:* AND labels.%s:*)",
		utils.SanitizeGCELabelValue(utils.LabelNodePoolKey),
		utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey))

	zones, err := p.getZonesInRegion(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "getting zones in region")
		return nil, fmt.Errorf("listing zones, %w", err)
	}

	for _, zone := range zones {
		req := p.computeService.Instances.List(p.projectID, zone).Filter(filter).Context(ctx)

		err := req.Pages(ctx, func(page *compute.InstanceList) error {
			if len(page.Items) == 0 {
				log.FromContext(ctx).Info("No instances found in zone", "zone", zone)
				return nil // empty page, continue
			}

			for _, inst := range page.Items {
				instances = append(instances, &Instance{
					InstanceID:   inst.Name,
					Name:         inst.Name,
					Type:         inst.MachineType[strings.LastIndex(inst.MachineType, "/")+1:], // just for matching "e2-standard-2"
					Location:     zone,
					ProjectID:    p.projectID,
					ImageID:      getBootImageID(inst),
					CreationTime: parseCreationTime(inst.CreationTimestamp),
					CapacityType: resolveCapacityType(inst.Scheduling),
					Labels:       inst.Labels,
					Tags:         inst.Labels,
					Status:       InstanceStatusRunning, // consider mapping from inst.Status
				})
			}
			return nil
		})

		if err != nil {
			log.FromContext(ctx).Error(err, "error listing instances in zone", "zone", zone)
			return nil, fmt.Errorf("listing instances in zone %s: %w", zone, err)
		}
	}

	log.FromContext(ctx).Info("finished listing GCP instances", "total", len(instances))
	return instances, nil
}

func (p *DefaultProvider) getZonesInRegion(ctx context.Context) ([]string, error) {
	region, err := p.computeService.Regions.Get(p.projectID, p.region).Context(ctx).Do()
	if err != nil {
		return nil, err
	}

	var zones []string
	for _, zoneURL := range region.Zones {
		parts := strings.Split(zoneURL, "/")
		zones = append(zones, parts[len(parts)-1])
	}
	return zones, nil
}

func (p *DefaultProvider) Delete(ctx context.Context, providerID string) error {
	project, zone, instance, err := parseGCEProviderID(providerID)
	if err != nil {
		return fmt.Errorf("parsing provider ID: %w", err)
	}

	log.FromContext(ctx).Info("Deleting instance", "project", project, "zone", zone, "instance", instance)

	op, err := p.computeService.Instances.Delete(project, zone, instance).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			log.FromContext(ctx).Info("Instance already deleted or not found", "instance", instance)
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance not found: %w", err))
		}
		return fmt.Errorf("deleting instance: %w", err)
	}

	log.FromContext(ctx).Info("Delete operation started", "operation", op.Name)
	return nil
}

func (p *DefaultProvider) CreateTags(ctx context.Context, providerID string, tags map[string]string) error {
	// TODO: Implement me
	return nil
}

func parseGCEProviderID(providerID string) (project, zone, instance string, err error) {
	const prefix = "gce://"
	if !strings.HasPrefix(providerID, prefix) {
		err = fmt.Errorf("unexpected providerID format: %s", providerID)
		return
	}

	parts := strings.Split(strings.TrimPrefix(providerID, prefix), "/")
	if len(parts) != 3 {
		err = fmt.Errorf("invalid GCE providerID, expected format gce://project/zone/instance")
		return
	}

	project, zone, instance = parts[0], parts[1], parts[2]
	return
}

func isInstanceNotFoundError(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == 404
	}
	return false
}
