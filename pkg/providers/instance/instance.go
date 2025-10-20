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
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const (
	maxInstanceTypes        = 20
	maxNodeCIDR             = 23
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
}

func NewProvider(clusterName, region, projectID, defaultServiceAccount string, computeService *compute.Service, gkeProvider gke.Provider,
	unavailableOfferings *pkgcache.UnavailableOfferings) Provider {
	return &DefaultProvider{
		gkeProvider:           gkeProvider,
		unavailableOfferings:  unavailableOfferings,
		instanceCache:         cache.New(instanceCacheExpiration, instanceCacheExpiration),
		clusterName:           clusterName,
		region:                region,
		projectID:             projectID,
		defaultServiceAccount: defaultServiceAccount,
		computeService:        computeService,
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

				errorMsgs := lo.Map(op.Error.Errors, func(e *compute.OperationErrorErrors, _ int) string {
					return e.Message
				})
				return fmt.Errorf("operation failed: %s", strings.Join(errorMsgs, "; "))
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

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error) {
	if len(instanceTypes) == 0 {
		return nil, fmt.Errorf("no instance types provided")
	}

	instanceTypes = orderInstanceTypesByPrice(instanceTypes, scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...))
	capacityType := p.getCapacityType(nodeClaim, instanceTypes)

	var errs []error
	// try all instance types, if one is available, use it
	for _, instanceType := range instanceTypes {
		zone, err := p.selectZone(ctx, nodeClaim, instanceType, capacityType)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to select zone for instance type", "instanceType", instanceType.Name)
			errs = append(errs, err)
			continue
		}

		nodePoolName := resolveNodePoolName(nodeClass.ImageFamily())
		if nodePoolName == "" {
			log.FromContext(ctx).Error(err, "failed to resolve node pool name for image family", "imageFamily", nodeClass.ImageFamily())
			return nil, fmt.Errorf("failed to resolve node pool name for image family %q", nodeClass.ImageFamily())
		}

		template, err := p.findTemplateByNodePoolName(ctx, nodePoolName)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to find template for alias", "alias", nodeClass.Spec.ImageSelectorTerms[0].Alias)
			errs = append(errs, err)
			continue
		}

		instanceName := fmt.Sprintf("karpenter-%s", nodeClaim.Name)
		// We need to check it in the following scenario:
		// 1. The instance is in the creation process.
		// 2. The pod is terminated
		// 3. The new pod will try to create the instance again, but it will fail because the instance is already in the creation process.
		instance, exists, err := p.isInstanceExists(ctx, zone, instanceName)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to check if instance exists", "instanceName", instanceName)
			return nil, fmt.Errorf("failed to check if instance exists: %w", err)
		}

		if !exists {
			instance = p.buildInstance(nodeClaim, nodeClass, instanceType, template, nodePoolName, zone, instanceName)
			op, err := p.computeService.Instances.Insert(p.projectID, zone, instance).Context(ctx).Do()
			if err != nil {
				log.FromContext(ctx).Error(err, "failed to create instance", "instanceType", instanceType.Name, "zone", zone)
				errs = append(errs, err)
				continue
			}

			if err := p.waitOperationDone(ctx, instanceType.Name, zone, capacityType, op.Name); err != nil {
				log.FromContext(ctx).Error(err, "failed to wait for operation to be done", "instanceType", instanceType.Name, "zone", zone)
				errs = append(errs, err)
				continue
			}
		}

		// we could wait for the node to be present in kubernetes api via csr sign up
		// should be done with watcher, for now implemented as a csr controller
		log.FromContext(ctx).Info("Created instance", "instanceName", instance.Name, "instanceType", instanceType.Name,
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

func resolveNodePoolName(imageFamily string) string {
	switch imageFamily {
	case v1alpha1.ImageFamilyContainerOptimizedOS:
		return nodepooltemplate.KarpenterDefaultNodePoolTemplate
	case v1alpha1.ImageFamilyUbuntu:
		return nodepooltemplate.KarpenterUbuntuNodePoolTemplate
	}

	return ""
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

func (p *DefaultProvider) buildInstance(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, template *compute.InstanceTemplate, nodePoolName, zone, instanceName string) *compute.Instance {
	attachedDisks, err := p.renderDiskProperties(instanceType, nodeClass, zone)
	if err != nil {
		log.FromContext(context.Background()).Error(err, "failed to render disk properties")
		return nil
	}

	capacityType := p.getCapacityType(nodeClaim, []*cloudprovider.InstanceType{instanceType})

	// Setup metadata
	if err := p.setupInstanceMetadata(template.Properties.Metadata, nodeClass, instanceType, nodeClaim, nodePoolName, capacityType); err != nil {
		log.FromContext(context.Background()).Error(err, "failed to setup instance metadata")
		return nil
	}

	// Setup service accounts
	serviceAccounts := p.setupServiceAccounts(nodeClass, template.Properties.ServiceAccounts)

	// setup network interfaces
	networkInterfaces := p.setupNetworkInterfaces(template, nodeClass)

	// Create instance
	instance := &compute.Instance{
		Name:              instanceName,
		MachineType:       fmt.Sprintf("zones/%s/machineTypes/%s", zone, instanceType.Name),
		Disks:             attachedDisks,
		NetworkInterfaces: networkInterfaces,
		ServiceAccounts:   serviceAccounts,
		Metadata:          template.Properties.Metadata,
		Labels:            p.initializeInstanceLabels(nodeClass),
		Scheduling:        template.Properties.Scheduling,
		Tags:              mergeInstanceTags(template.Properties.Tags, nodeClass.Spec.NetworkTags),
	}

	// Configure capacity provision
	p.configureInstanceCapacityProvision(instance, capacityType)

	// Setup karpenter built-in labels
	p.setupInstanceLabels(instance, nodeClaim, nodeClass, instanceType)

	return instance
}

// nolint:gocyclo
func (p *DefaultProvider) setupNetworkInterfaces(template *compute.InstanceTemplate, nodeClass *v1alpha1.GCENodeClass) []*compute.NetworkInterface {
	// referring to: https://cloud.google.com/kubernetes-engine/docs/how-to/flexible-pod-cidr
	maxPods := nodeClass.GetMaxPods()
	targetRange := int32(maxNodeCIDR)
	if maxPods <= 8 {
		targetRange = 28
	}
	if maxPods >= 9 && maxPods <= 16 {
		targetRange = 27
	}
	if maxPods >= 17 && maxPods <= 32 {
		targetRange = 26
	}
	if maxPods >= 33 && maxPods <= 64 {
		targetRange = 25
	}
	if maxPods >= 65 && maxPods <= 128 {
		targetRange = 24
	}
	if maxPods >= 129 && maxPods <= 256 {
		targetRange = 23
	}

	var networkInterfaces []*compute.NetworkInterface

	for _, networkInterface := range template.Properties.NetworkInterfaces {
		tmpNetworkInterface := &compute.NetworkInterface{
			AccessConfigs:            networkInterface.AccessConfigs,
			AliasIpRanges:            networkInterface.AliasIpRanges,
			Fingerprint:              networkInterface.Fingerprint,
			InternalIpv6PrefixLength: networkInterface.InternalIpv6PrefixLength,
			Ipv6AccessConfigs:        networkInterface.Ipv6AccessConfigs,
			Ipv6AccessType:           networkInterface.Ipv6AccessType,
			Ipv6Address:              networkInterface.Ipv6Address,
			Kind:                     networkInterface.Kind,
			Name:                     networkInterface.Name,
			Network:                  networkInterface.Network,
			NetworkIP:                networkInterface.NetworkIP,
			NicType:                  networkInterface.NicType,
			QueueCount:               networkInterface.QueueCount,
			StackType:                networkInterface.StackType,
			Subnetwork:               networkInterface.Subnetwork,
			ForceSendFields:          networkInterface.ForceSendFields,
			NullFields:               networkInterface.NullFields,
		}
		for aliasIpRangeIndex := range networkInterface.AliasIpRanges {
			// Set the IP CIDR range for the alias IP range based on the calculated targetRange
			// TODO: Optionally, add validation to ensure the network interface supports this range if needed
			tmpNetworkInterface.AliasIpRanges[aliasIpRangeIndex].IpCidrRange = fmt.Sprintf("/%d", targetRange)
		}
		networkInterfaces = append(networkInterfaces, tmpNetworkInterface)
	}

	return networkInterfaces
}

// setupInstanceMetadata configures all metadata-related settings for the instance
func (p *DefaultProvider) setupInstanceMetadata(instanceMetadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, nodeClaim *karpv1.NodeClaim, nodePoolName string, capacityType string) error {
	if err := metadata.RemoveGKEBuiltinLabels(instanceMetadata, nodePoolName); err != nil {
		return fmt.Errorf("failed to remove GKE builtin labels from metadata: %w", err)
	}

	if err := metadata.SetMaxPodsPerNode(instanceMetadata, nodeClass); err != nil {
		return fmt.Errorf("failed to set max pods per node in metadata: %w", err)
	}

	if err := metadata.RenderKubeletConfigMetadata(instanceMetadata, instanceType); err != nil {
		return fmt.Errorf("failed to render kubelet config metadata: %w", err)
	}

	if err := metadata.PatchUnregisteredTaints(instanceMetadata); err != nil {
		return fmt.Errorf("failed to append unregistered taint to kube-env: %w", err)
	}

	if capacityType == karpv1.CapacityTypeSpot {
		if err := metadata.SetProvisioningModel(instanceMetadata, capacityType); err != nil {
			return fmt.Errorf("failed to set provisioning model in metadata: %w", err)
		}
	}

	metadata.AppendNodeClaimLabel(nodeClaim, nodeClass, instanceMetadata)
	metadata.AppendRegisteredLabel(instanceMetadata)

	return nil
}

// setupServiceAccounts configures service accounts for the instance
func (p *DefaultProvider) setupServiceAccounts(nodeClass *v1alpha1.GCENodeClass, defaultServiceAccounts []*compute.ServiceAccount) []*compute.ServiceAccount {
	serviceAccount := p.resolveServiceAccount(nodeClass)
	if serviceAccount == "" {
		return defaultServiceAccounts
	}

	return []*compute.ServiceAccount{
		{
			Email: serviceAccount,
			Scopes: []string{
				// cloud-platform scope provides full access to all Google Cloud Platform APIs
				// Note: This is a broad scope
				// However, since GCENodeClass doesn't support custom OAuth scopes configuration,
				// we use this as a compromise to ensure the instance has necessary permissions
				// for basic operations like VM management, storage access, and container registry access.
				// TODO: When NodeClass API supports custom OAuth scopes, replace with more restrictive scopes
				"https://www.googleapis.com/auth/cloud-platform",
			},
		},
	}
}

// initializeInstanceLabels initializes the instance labels map
func (p *DefaultProvider) initializeInstanceLabels(nodeClass *v1alpha1.GCENodeClass) map[string]string {
	labels := make(map[string]string)
	for k, v := range nodeClass.Spec.Labels {
		labels[k] = v
	}
	return labels
}

// configureInstanceCapacityProvision configures capacity provision settings for the instance
func (p *DefaultProvider) configureInstanceCapacityProvision(instance *compute.Instance, capacityType string) {
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

// setupInstanceLabels configures all labels for the instance
func (p *DefaultProvider) setupInstanceLabels(instance *compute.Instance, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType) {
	// Set common Karpenter labels
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelNodePoolKey)] = nodeClaim.Labels[karpv1.NodePoolLabelKey]
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey)] = nodeClass.Name
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterNameKey)] = p.clusterName

	// Add instance type requirement labels
	lo.ForEach(lo.Entries(instanceType.Requirements.Labels()), func(entry lo.Entry[string, string], _ int) {
		instance.Labels[entry.Key] = entry.Value
	})
}

func mergeInstanceTags(templateTags *compute.Tags, networkTags []string) *compute.Tags {
	if (templateTags == nil || len(templateTags.Items) == 0) && len(networkTags) == 0 {
		return nil
	}

	templateLen := 0
	if templateTags != nil {
		templateLen = len(templateTags.Items)
	}

	merged := make([]string, 0, templateLen+len(networkTags))

	if templateLen > 0 {
		merged = append(merged, templateTags.Items...)
	}

	if len(networkTags) > 0 {
		merged = append(merged, networkTags...)
	}

	if len(merged) == 0 {
		return nil
	}

	newTags := &compute.Tags{Items: merged}
	if templateTags != nil {
		newTags.Fingerprint = templateTags.Fingerprint
	}

	return newTags
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
