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
	"sort"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const (
	maxInstanceTypes        = 20
	instanceCacheExpiration = 5 * time.Minute // Cache expiration for instances
)

type Provider interface {
	Create(context.Context, *v1alpha1.GCENodeClass, *karpv1.NodeClaim, []*cloudprovider.InstanceType) (*Instance, error)
	Get(context.Context, string) (*Instance, error)
	List(context.Context) ([]*Instance, error)
	Delete(context.Context, string) error
	CreateTags(context.Context, string, map[string]string) error
}

type DefaultProvider struct {
	// In current implementation, instanceID == InstanceName
	instanceCache *cache.Cache

	clusterName    string
	region         string
	projectID      string
	computeService *compute.Service
}

func NewProvider(clusterName, region, projectID string, computeService *compute.Service) *DefaultProvider {
	return &DefaultProvider{
		instanceCache:  cache.New(instanceCacheExpiration, instanceCacheExpiration),
		clusterName:    clusterName,
		region:         region,
		projectID:      projectID,
		computeService: computeService,
	}
}

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error) {
	if len(instanceTypes) == 0 {
		return nil, fmt.Errorf("no instance types provided")
	}

	capacityType := p.getCapacityType(nodeClaim, instanceTypes)

	instanceType, err := p.selectInstanceType(nodeClaim, instanceTypes)
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
	log.FromContext(ctx).Info("Created instance", "instanceName", op.Name, "instanceType", instanceType.Name,
		"zone", zone, "projectID", p.projectID, "region", p.region, "providerID", instance.Name, "providerID", instance.Name,
		"Labels", instance.Labels, "Tags", instance.Tags, "Status", instance.Status)

	return &Instance{
		InstanceID: instance.Name,
		Name:       instance.Name,
		// Refer to https://github.com/cloudpilot-ai/karpenter-provider-gcp/pull/45#discussion_r2115586327
		// In this develop period, we are using a static instance type to avoid high cost of creating a new instance type for each node claim.
		// Type:         instanceType.Name,
		Type:         instanceType.Name,
		Location:     zone,
		ProjectID:    p.projectID,
		ImageID:      template.Properties.Disks[0].InitializeParams.SourceImage,
		CreationTime: time.Now(),
		CapacityType: capacityType,
		Tags:         template.Properties.Labels,
		Labels:       instance.Labels,
		Status:       InstanceStatusProvisioning,
	}, nil
}

// getCapacityType selects spot if both constraints are flexible and there is an
// available offering.
func (p *DefaultProvider) getCapacityType(nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) string {
	requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	if requirements.Get(karpv1.CapacityTypeLabelKey).Has(karpv1.CapacityTypeSpot) {
		requirements[karpv1.CapacityTypeLabelKey] = scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot)
		for _, instanceType := range instanceTypes {
			for _, offering := range instanceType.Offerings.Available() {
				if requirements.Compatible(offering.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil {
					return karpv1.CapacityTypeSpot
				}
			}
		}
	}
	return karpv1.CapacityTypeOnDemand
}

func (p *DefaultProvider) selectInstanceType(nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*cloudprovider.InstanceType, error) {
	schedulingRequirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	instanceTypes, err := cloudprovider.InstanceTypes(instanceTypes).Truncate(schedulingRequirements, maxInstanceTypes)
	if err != nil {
		return nil, fmt.Errorf("truncating instance types, %w", err)
	}

	if len(instanceTypes) == 0 {
		return nil, fmt.Errorf("no instance types available after truncation")
	}

	// Sort instance types by max offerings
	sort.Slice(instanceTypes, func(i, j int) bool {
		return len(instanceTypes[i].Offerings) > len(instanceTypes[j].Offerings)
	})

	// Return the cheapest instance type
	return instanceTypes[0], nil
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

//nolint:gocyclo
func (p *DefaultProvider) findTemplateForAlias(ctx context.Context, alias string) (*compute.InstanceTemplate, error) {
	if alias == "" {
		return nil, fmt.Errorf("alias not specified in ImageSelectorTerm")
	}

	var expectedLabelValue string
	switch alias {
	case nodepooltemplate.KarpenterDefaultNodePoolTemplateAlias:
		expectedLabelValue = nodepooltemplate.KarpenterDefaultNodePoolTemplate
	case nodepooltemplate.KarpenterUbuntuNodePoolTemplateAlias:
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
			// instanceTemplates are shared across clusters, so we need to check if the template belongs to the current cluster
			// This is done by checking the metadata for the cluster name label.
			if t.Properties.Metadata != nil {
				metadataClusterNamemetadata, err := metadata.GetClusterName(t.Properties.Metadata)
				if err != nil {
					log.FromContext(ctx).Error(err, "failed to get cluster name from metadata", "instanceTemplate", t.Name)
					continue
				}
				if metadataClusterNamemetadata != p.clusterName {
					log.FromContext(ctx).Info("Skipping instance template from different cluster", "templateName", t.Name, "clusterName", p.clusterName)
					continue
				}
			}
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

	err := metadata.RemoveGKEBuiltinLabels(template.Properties.Metadata)
	if err != nil {
		log.FromContext(context.Background()).Error(err, "failed to remove GKE builtin labels from metadata")
		return nil
	}

	err = metadata.RenderKubeletConfigMetadata(template.Properties.Metadata, instanceType)
	if err != nil {
		log.FromContext(context.Background()).Error(err, "failed to render kubelet config metadata")
		return nil
	}

	err = metadata.PatchUnregisteredTaints(template.Properties.Metadata)
	if err != nil {
		log.FromContext(context.Background()).Error(err, "failed to append unregistered taint to kube-env")
		return nil
	}

	metadata.AppendNodeclaimLabel(nodeClaim, nodeClass, template.Properties.Metadata)
	metadata.AppendRegisteredLabel(template.Properties.Metadata)

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
	capacityType := p.getCapacityType(nodeClaim, []*cloudprovider.InstanceType{instanceType})
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
	lo.ForEach(lo.Entries(instanceType.Requirements.Labels()), func(entry lo.Entry[string, string], _ int) {
		instance.Labels[entry.Key] = entry.Value
	})

	return instance
}

func (p *DefaultProvider) Get(ctx context.Context, providerID string) (*Instance, error) {
	project, zone, instanceName, err := parseGCEProviderID(providerID)
	if err != nil {
		return nil, fmt.Errorf("parsing provider ID: %w", err)
	}

	if instance, ok := p.instanceCache.Get(instanceName); ok {
		return instance.(*Instance), nil
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

	p.syncInstance(instance)

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
		utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey),
	)

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
				instance := &Instance{
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
				}
				p.syncInstance(instance)
				instances = append(instances, instance)
			}
			return nil
		})

		if err != nil {
			log.FromContext(ctx).Error(err, "error listing instances in zone", "zone", zone)
			return nil, fmt.Errorf("listing instances in zone %s: %w", zone, err)
		}
	}

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
	projectID, zone, instanceName, err := parseGCEProviderID(providerID)
	if err != nil {
		return fmt.Errorf("failed to parse provider ID: %w", err)
	}

	instance, err := p.computeService.Instances.Get(projectID, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			log.FromContext(ctx).Info("Instance not found", "instance", instanceName)
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance not found: %w", err))
		}
		return fmt.Errorf("getting instance: %w", err)
	}

	newLabels := instance.Labels
	if newLabels == nil {
		newLabels = make(map[string]string)
	}
	for k, v := range tags {
		newLabels[k] = v
	}

	req := &compute.InstancesSetLabelsRequest{
		Labels:           newLabels,
		LabelFingerprint: instance.LabelFingerprint,
	}

	_, err = p.computeService.Instances.SetLabels(projectID, zone, instanceName, req).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("setting labels: %w", err)
	}

	// List and sync instances to update the cache
	_, err = p.List(ctx)
	if err != nil {
		return err
	}

	return err
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

func (p *DefaultProvider) syncInstance(instance *Instance) {
	p.instanceCache.Set(instance.InstanceID, instance, cache.DefaultExpiration)
}
