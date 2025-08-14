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

package instance

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const (
	maxInstanceTypes        = 20
	instanceCacheExpiration = 15 * time.Second
)

var (
	InsufficientCapacityErrorCodes = sets.NewString("ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS", "ZONE_RESOURCE_POOL_EXHAUSTED")
)

type Provider interface {
	Create(context.Context, *v1alpha1.GCENodeClass, *karpv1.NodeClaim, []*cloudprovider.InstanceType) (*Instance, error)
	Get(context.Context, string) (*Instance, error)
	List(context.Context) ([]*Instance, error)
	Delete(context.Context, string) error
	CreateTags(context.Context, string, map[string]string) error
}

type DefaultProvider struct {
	gkeProvider          gke.Provider
	unavailableOfferings *pkgcache.UnavailableOfferings

	// In current implementation, instanceID == InstanceName
	instanceCache *cache.Cache

	clusterName           string
	region                string
	projectID             string
	defaultServiceAccount string
	computeService        *compute.Service
	kubeClient            client.Client
}

func NewProvider(clusterName, region, projectID, defaultServiceAccount string, computeService *compute.Service, gkeProvider gke.Provider,
	unavailableOfferings *pkgcache.UnavailableOfferings, kubeClient client.Client) Provider {
	return &DefaultProvider{
		gkeProvider:           gkeProvider,
		unavailableOfferings:  unavailableOfferings,
		instanceCache:         cache.New(instanceCacheExpiration, instanceCacheExpiration),
		clusterName:           clusterName,
		region:                region,
		projectID:             projectID,
		defaultServiceAccount: defaultServiceAccount,
		computeService:        computeService,
		kubeClient:            kubeClient,
	}
}

func (p *DefaultProvider) waitOperationDone(ctx context.Context,
	instanceType, zone, capacityType, operationName string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	timeout := time.NewTimer(10 * time.Second)

	for {
		select {
		case <-timeout.C:
			// if the operation does not finish in 10s, it means there is enough resources and the creation will be successful
			return nil
		case <-ticker.C:
			op, err := p.computeService.ZoneOperations.Get(p.projectID, zone, operationName).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("getting operation: %w", err)
			}

			if op.Status != "DONE" {
				continue
			}

			if op.Error != nil {
				capacityError, found := lo.Find(op.Error.Errors, func(e *compute.OperationErrorErrors) bool {
					// Example in real environment:
					// Error: ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS The zone 'projects/project-id/zones/us-west1-a' does not have enough resources available to fulfill the request.  '(resource type:compute)'.
					return InsufficientCapacityErrorCodes.Has(e.Code)
				})
				if found {
					p.unavailableOfferings.MarkUnavailable(ctx, capacityError.Message, instanceType, zone, capacityType)
					return cloudprovider.NewInsufficientCapacityError(fmt.Errorf("zone resource pool exhausted: %s", capacityError.Message))
				}
				return fmt.Errorf("operation failed: %v", op.Error.Errors)
			}
			return nil
		}
	}
}

func (p *DefaultProvider) isInstanceExists(ctx context.Context, zone, instanceName string) (*compute.Instance, bool, error) {
	instance, err := p.computeService.Instances.Get(p.projectID, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get instance: %w", err)
	}
	return instance, true, nil
}

func (p *DefaultProvider) launchInstance(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, template *compute.InstanceTemplate, zone, instanceName, capacityType string) (*compute.Instance, error) {
	instance := p.buildInstance(nodeClaim, nodeClass, instanceType, template, zone, instanceName)
	op, err := p.computeService.Instances.Insert(p.projectID, zone, instance).Context(ctx).Do()
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create instance", "instanceType", instanceType.Name, "zone", zone)
		return nil, err
	}

	if err := p.waitOperationDone(ctx, instanceType.Name, zone, capacityType, op.Name); err != nil {
		log.FromContext(ctx).Error(err, "failed to wait for operation to be done", "instanceType", instanceType.Name, "zone", zone)
		return nil, err
	}

	nodeClaim.Status.ProviderID = fmt.Sprintf("gce://%s/%s/%s", p.projectID, zone, instanceName)
	return instance, nil
}

func (p *DefaultProvider) createInstance(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceType *cloudprovider.InstanceType, capacityType string) (*Instance, error) {
	zone, err := p.selectZone(ctx, nodeClaim, instanceType, capacityType)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to select zone for instance type", "instanceType", instanceType.Name)
		return nil, err
	}

	nodePoolName := utils.ResolveNodePoolName(nodeClass.Name)
	if nodePoolName == "" {
		log.FromContext(ctx).Error(err, "failed to resolve node pool name for image family", "imageFamily", nodeClass.ImageFamily())
		return nil, fmt.Errorf("failed to resolve node pool name for image family %q", nodeClass.ImageFamily())
	}

	template, err := p.findTemplateByNodePoolName(ctx, nodePoolName)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to find template for alias", "alias", nodeClass.Spec.ImageSelectorTerms[0].Alias)
		return nil, err
	}

	instanceName := fmt.Sprintf("karpenter-%s", nodeClaim.Name)
	instance, exists, err := p.isInstanceExists(ctx, zone, instanceName)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to check if instance exists", "instanceName", instanceName)
		return nil, fmt.Errorf("failed to check if instance exists: %w", err)
	}

	if !exists {
		instance, err = p.launchInstance(ctx, nodeClaim, nodeClass, instanceType, template, zone, instanceName, capacityType)
		if err != nil {
			return nil, err
		}
	}

	log.FromContext(ctx).Info("Created instance", "instanceName", instance.Name, "instanceType", instanceType.Name,
		"zone", zone, "projectID", p.projectID, "region", p.region, "providerID", instance.Name, "providerID", instance.Name,
		"Labels", instance.Labels, "Tags", instance.Tags, "Status", instance.Status)

	return &Instance{
		InstanceID:   instance.Name,
		Name:         instance.Name,
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

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error) {
	if len(instanceTypes) == 0 {
		return nil, fmt.Errorf("no instance types provided")
	}

	instanceTypes = orderInstanceTypesByPrice(instanceTypes, scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...))
	capacityType := p.getCapacityType(nodeClaim, instanceTypes)

	var errs []error
	// try all instance types, if one is available, use it
	for _, instanceType := range instanceTypes {
		instance, err := p.createInstance(ctx, nodeClass, nodeClaim, instanceType, capacityType)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return instance, nil
	}

	return nil, fmt.Errorf("failed to create instance after trying all instance types: %w", errors.Join(errs...))
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

func orderInstanceTypesByPrice(instanceTypes []*cloudprovider.InstanceType, requirements scheduling.Requirements) []*cloudprovider.InstanceType {
	// Order instance types so that we get the cheapest instance types of the available offerings
	sort.Slice(instanceTypes, func(i, j int) bool {
		iPrice := math.MaxFloat64
		jPrice := math.MaxFloat64
		if len(instanceTypes[i].Offerings.Available().Compatible(requirements)) > 0 {
			iPrice = instanceTypes[i].Offerings.Available().Compatible(requirements).Cheapest().Price
		}
		if len(instanceTypes[j].Offerings.Available().Compatible(requirements)) > 0 {
			jPrice = instanceTypes[j].Offerings.Available().Compatible(requirements).Cheapest().Price
		}
		if iPrice == jPrice {
			return instanceTypes[i].Name < instanceTypes[j].Name
		}
		return iPrice < jPrice
	})
	return instanceTypes
}

// zone should be based on the offering, for now lets return static zone from requirements
func (p *DefaultProvider) selectZone(ctx context.Context, nodeClaim *karpv1.NodeClaim,
	instanceType *cloudprovider.InstanceType, capacityType string) (string, error) {
	zones, err := p.gkeProvider.ResolveClusterZones(ctx)
	if err != nil {
		return "", err
	}

	cheapestZone := ""
	cheapestPrice := math.MaxFloat64
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	if capacityType == karpv1.CapacityTypeOnDemand {
		// For on-demand, randomly select a zone
		if len(zones) > 0 {
			randomIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(zones))))
			return zones[randomIndex.Int64()], nil
		}
	}

	zonesSet := sets.NewString(zones...)
	// For different AZ, the spot price may differ. So we need to get the cheapest vSwitch in the zone
	for _, offering := range instanceType.Offerings {
		if !offering.Available {
			continue
		}
		if reqs.Compatible(offering.Requirements, scheduling.AllowUndefinedWellKnownLabels) != nil {
			continue
		}
		ok := zonesSet.Has(offering.Requirements.Get(corev1.LabelTopologyZone).Any())
		if !ok {
			continue
		}
		if offering.Price < cheapestPrice {
			cheapestZone = offering.Requirements.Get(corev1.LabelTopologyZone).Any()
			cheapestPrice = offering.Price
		}
	}

	return cheapestZone, nil
}

//nolint:gocyclo
func (p *DefaultProvider) findTemplateByNodePoolName(ctx context.Context, nodePoolName string) (*compute.InstanceTemplate, error) {
	if nodePoolName == "" {
		return nil, fmt.Errorf("node pool name not specified in ImageSelectorTerm")
	}

	instanceTemplates, err := p.computeService.RegionInstanceTemplates.List(p.projectID, p.region).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("cannot list all instance templates for node pool name %q: %w", nodePoolName, err)
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
					continue
				}
			}
			if val, ok := t.Properties.Labels["goog-k8s-node-pool-name"]; ok && val == nodePoolName {
				log.FromContext(ctx).Info("Found instance template", "templateName", t.Name, "clusterName", p.clusterName)
				return t, nil
			}
		}
	}

	return nil, fmt.Errorf("no instance template found with label goog-k8s-node-pool-name=%s", nodePoolName)
}

func (p *DefaultProvider) renderDiskProperties(instanceType *cloudprovider.InstanceType,
	nodeClass *v1alpha1.GCENodeClass, zone string) ([]*compute.AttachedDisk, error) {
	disks := nodeClass.Spec.Disks
	sort.Slice(disks, func(i, j int) bool {
		return disks[i].Boot
	})

	attachedDisks := make([]*compute.AttachedDisk, len(disks))
	for i, disk := range disks {
		// Create a new disk configuration for each disk to avoid sharing references
		attachedDisk := &compute.AttachedDisk{
			AutoDelete: true,
			Boot:       disk.Boot,
			InitializeParams: &compute.AttachedDiskInitializeParams{
				DiskType:   fmt.Sprintf("projects/%s/zones/%s/diskTypes/%s", p.projectID, zone, disk.Category),
				DiskSizeGb: int64(disk.SizeGiB),
			},
		}

		requirements := instanceType.Requirements
		if requirements.Get(corev1.LabelArchStable).Has(imagefamily.OSArchARM64Requirement) {
			attachedDisk.InitializeParams.Architecture = imagefamily.OSArchitectureARM
		}

		if disk.Boot {
			targetImage, found := lo.Find(nodeClass.Status.Images, func(image v1alpha1.Image) bool {
				reqs := scheduling.NewNodeSelectorRequirements(image.Requirements...)
				return reqs.Compatible(instanceType.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil
			})
			if !found {
				return nil, fmt.Errorf("no target image found for node class %s", nodeClass.Name)
			}
			attachedDisk.InitializeParams.SourceImage = targetImage.SourceImage
		}

		attachedDisks[i] = attachedDisk
	}

	return attachedDisks, nil
}

func (p *DefaultProvider) configureScheduling(instance *compute.Instance, nodeClaim *karpv1.NodeClaim, instanceType *cloudprovider.InstanceType) {
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
}

func (p *DefaultProvider) configureLabelsAndTags(instance *compute.Instance, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceType *cloudprovider.InstanceType) {
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelNodePoolKey)] = nodeClaim.Labels[karpv1.NodePoolLabelKey]
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey)] = nodeClass.Name
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterNameKey)] = p.clusterName
	lo.ForEach(lo.Entries(instanceType.Requirements.Labels()), func(entry lo.Entry[string, string], _ int) {
		instance.Labels[entry.Key] = entry.Value
	})

	if instance.Tags == nil {
		instance.Tags = &compute.Tags{}
	}
	instance.Tags.Items = append(instance.Tags.Items, nodeClass.Spec.NetworkTags...)

	if nodeClass.Spec.Tags != nil {
		for k, v := range nodeClass.Spec.Tags {
			instance.Labels[k] = v
		}
	}
}

func (p *DefaultProvider) configureMetadata(ctx context.Context, templateMetadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceType *cloudprovider.InstanceType) error {
	if err := metadata.SetMaxPodsPerNode(templateMetadata, nodeClass); err != nil {
		log.FromContext(ctx).Error(err, "failed to set max pods per node in metadata")
		return err
	}
	if err := metadata.RenderKubeletConfigMetadata(templateMetadata, instanceType); err != nil {
		log.FromContext(ctx).Error(err, "failed to render kubelet config metadata")
		return err
	}
	if err := metadata.PatchUnregisteredTaints(templateMetadata); err != nil {
		log.FromContext(ctx).Error(err, "failed to append unregistered taint to kube-env")
		return err
	}
	metadata.AppendNodeclaimLabel(nodeClaim, nodeClass, templateMetadata)
	metadata.AppendRegisteredLabel(templateMetadata)
	return nil
}

func (p *DefaultProvider) buildInstance(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, template *compute.InstanceTemplate, zone, instanceName string) *compute.Instance {
	attachedDisks, err := p.renderDiskProperties(instanceType, nodeClass, zone)
	if err != nil {
		log.FromContext(context.Background()).Error(err, "failed to render disk properties")
		return nil
	}

	if err := p.configureMetadata(context.Background(), template.Properties.Metadata, nodeClass, nodeClaim, instanceType); err != nil {
		return nil
	}

	serviceAccount := p.resolveServiceAccount(nodeClass)
	serviceAccounts := template.Properties.ServiceAccounts
	if serviceAccount != "" {
		serviceAccounts = []*compute.ServiceAccount{
			{
				Email: serviceAccount,
			},
		}
	}

	instance := &compute.Instance{
		Name:              instanceName,
		MachineType:       fmt.Sprintf("zones/%s/machineTypes/%s", zone, instanceType.Name),
		Disks:             attachedDisks,
		NetworkInterfaces: template.Properties.NetworkInterfaces,
		ServiceAccounts:   serviceAccounts,
		Metadata:          template.Properties.Metadata,
		Labels:            map[string]string{},
		Scheduling:        template.Properties.Scheduling,
		Tags:              template.Properties.Tags,
	}

	p.configureScheduling(instance, nodeClaim, instanceType)
	p.configureLabelsAndTags(instance, nodeClass, nodeClaim, instanceType)

	return instance
}

func (p *DefaultProvider) Get(ctx context.Context, providerID string) (*Instance, error) {
	_, _, instanceName, err := parseGCEProviderID(providerID)
	if err != nil {
		return nil, fmt.Errorf("parsing provider ID: %w", err)
	}

	if instance, ok := p.instanceCache.Get(instanceName); ok {
		return instance.(*Instance), nil
	}

	if err := p.syncInstances(ctx); err != nil {
		log.FromContext(ctx).Error(err, "syncing instances")
		return nil, err
	}

	currentInstance, ok := p.instanceCache.Get(instanceName)
	if ok {
		return currentInstance.(*Instance), nil
	}

	return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance(%s) not found ", providerID))
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
	if err := p.syncInstances(ctx); err != nil {
		log.FromContext(ctx).Error(err, "syncing instances failed")
		return nil, err
	}

	var instances []*Instance
	for _, instance := range p.instanceCache.Items() {
		instances = append(instances, instance.Object.(*Instance))
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

func (p *DefaultProvider) syncInstances(ctx context.Context) error {
	var instances []*Instance
	filter := fmt.Sprintf(
		"(labels.%s:* AND labels.%s:* AND labels.%s:%s)",
		utils.SanitizeGCELabelValue(utils.LabelNodePoolKey),
		utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey),
		utils.SanitizeGCELabelValue(utils.LabelClusterNameKey),
		p.clusterName,
	)

	zones, err := p.getZonesInRegion(ctx)
	if err != nil {
		return fmt.Errorf("listing zones, %w", err)
	}

	for _, zone := range zones {
		req := p.computeService.Instances.List(p.projectID, zone).Filter(filter).Context(ctx)

		err := req.Pages(ctx, func(page *compute.InstanceList) error {
			if len(page.Items) == 0 {
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
					Status:       inst.Status,
				}
				instances = append(instances, instance)
			}
			return nil
		})

		if err != nil {
			return fmt.Errorf("listing instances in zone %s: %w", zone, err)
		}
	}

	for _, instance := range instances {
		p.instanceCache.Set(instance.InstanceID, instance, cache.DefaultExpiration)
	}
	return nil
}

// resolveServiceAccount returns the service account to use for the instance.
// If the NodeClass specifies a service account, use that. Otherwise, use the default.
// Returns empty string if neither is specified.
func (p *DefaultProvider) resolveServiceAccount(nodeClass *v1alpha1.GCENodeClass) string {
	if nodeClass.Spec.ServiceAccount != "" {
		return nodeClass.Spec.ServiceAccount
	}
	return p.defaultServiceAccount
}

func (p *DefaultProvider) waitForNodeReady(ctx context.Context, instanceName string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	timeout := time.NewTimer(5 * time.Minute)
	defer timeout.Stop()

	for {
		select {
		case <-timeout.C:
			return fmt.Errorf("timed out waiting for node to be ready")
		case <-ticker.C:
			var node corev1.Node
			if err := p.kubeClient.Get(ctx, client.ObjectKey{Name: instanceName}, &node); err != nil {
				if k8serrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("getting node: %w", err)
			}

			for _, condition := range node.Status.Conditions {
				if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
					return nil
				}
			}
		}
	}
}
