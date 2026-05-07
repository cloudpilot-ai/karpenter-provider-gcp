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
	"google.golang.org/api/container/v1"
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
	maxInstanceTypes               = 20
	maxNodeCIDR                    = 23
	instanceCacheExpiration        = 15 * time.Second
	zoneOperationPollInterval      = 1 * time.Second
	defaultZoneOperationTimeout    = 2 * time.Minute
	ipSpaceInsufficientCapacityTTL = 30 * time.Second

	instanceTerminationActionDelete = "DELETE"
)

var InsufficientCapacityErrorCodes = sets.NewString(
	"ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS",
	"ZONE_RESOURCE_POOL_EXHAUSTED",
	"IP_SPACE_EXHAUSTED_WITH_DETAILS",
	"IP_SPACE_EXHAUSTED",
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
	clusterLocation       string
	region                string
	projectID             string
	defaultServiceAccount string
	computeDefaultSA      string
	computeService        *compute.Service
}

func NewProvider(clusterName, clusterLocation, region, projectID, defaultServiceAccount, computeDefaultSA string, computeService *compute.Service, gkeProvider gke.Provider,
	unavailableOfferings *pkgcache.UnavailableOfferings,
) Provider {
	return &DefaultProvider{
		gkeProvider:           gkeProvider,
		unavailableOfferings:  unavailableOfferings,
		instanceCache:         cache.New(instanceCacheExpiration, instanceCacheExpiration),
		clusterName:           clusterName,
		clusterLocation:       clusterLocation,
		region:                region,
		projectID:             projectID,
		defaultServiceAccount: defaultServiceAccount,
		computeDefaultSA:      computeDefaultSA,
		computeService:        computeService,
	}
}

func (p *DefaultProvider) waitOperationDone(ctx context.Context,
	instanceType, zone, capacityType, operationName string,
) error {
	waitCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		waitCtx, cancel = context.WithTimeout(ctx, defaultZoneOperationTimeout)
	}
	defer cancel()

	ticker := time.NewTicker(zoneOperationPollInterval)
	defer ticker.Stop()

	for {
		op, err := p.computeService.ZoneOperations.Get(p.projectID, zone, operationName).Context(waitCtx).Do()
		if err != nil {
			if waitCtx.Err() != nil {
				return fmt.Errorf("waiting for operation %s: %w", operationName, waitCtx.Err())
			}
			if isTransientError(err) {
				log.FromContext(ctx).V(1).Info("transient error polling operation, retrying",
					"operation", operationName, "zone", zone, "error", err)
				if tickErr := waitForNextTick(waitCtx, ticker); tickErr != nil {
					return fmt.Errorf("waiting for operation %s: %w", operationName, tickErr)
				}
				continue
			}
			return fmt.Errorf("getting operation: %w", err)
		}

		if op.Status == "DONE" {
			if op.Error != nil {
				return p.handleZoneOperationError(waitCtx, op, instanceType, zone, capacityType)
			}
			return nil
		}

		if err := waitForNextTick(waitCtx, ticker); err != nil {
			return fmt.Errorf("waiting for operation %s: %w", operationName, err)
		}
	}
}

func waitForNextTick(ctx context.Context, ticker *time.Ticker) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ticker.C:
		return nil
	}
}

func (p *DefaultProvider) handleZoneOperationError(ctx context.Context, op *compute.Operation, instanceType, zone, capacityType string) error {
	capacityError, found := lo.Find(op.Error.Errors, func(e *compute.OperationErrorErrors) bool {
		return isInsufficientCapacityError(e)
	})
	if found {
		reason := capacityError.Message
		if reason == "" {
			reason = capacityError.Code
		}
		ttl := insufficientCapacityBackoffTTL(capacityError.Code)
		p.unavailableOfferings.MarkUnavailableWithTTL(ctx, reason, instanceType, zone, capacityType, ttl)
		return cloudprovider.NewInsufficientCapacityError(fmt.Errorf("zone %s insufficient capacity: %s", zone, reason))
	}

	errorMsgs := lo.Map(op.Error.Errors, func(e *compute.OperationErrorErrors, _ int) string {
		return e.Message
	})
	return fmt.Errorf("operation failed: %s", strings.Join(errorMsgs, "; "))
}

func isTransientError(err error) bool {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code >= 500
	}
	return false
}

func isInsufficientCapacityError(operationError *compute.OperationErrorErrors) bool {
	if operationError == nil {
		return false
	}

	return InsufficientCapacityErrorCodes.Has(operationError.Code)
}

func extractInsertInsufficientCapacityReason(err error) (string, string, bool) {
	var apiError *googleapi.Error
	if !errors.As(err, &apiError) {
		return "", "", false
	}

	for _, detail := range apiError.Errors {
		if InsufficientCapacityErrorCodes.Has(detail.Reason) {
			reason := detail.Message
			if reason == "" {
				reason = detail.Reason
			}
			return reason, detail.Reason, true
		}
	}

	return "", "", false
}

func insufficientCapacityBackoffTTL(reasonCode string) time.Duration {
	if reasonCode == "IP_SPACE_EXHAUSTED_WITH_DETAILS" || reasonCode == "IP_SPACE_EXHAUSTED" {
		return ipSpaceInsufficientCapacityTTL
	}

	return pkgcache.UnavailableOfferingsTTL
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

func (p *DefaultProvider) findInstanceByNodeClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*compute.Instance, error) {
	instanceName := fmt.Sprintf("karpenter-%s", nodeClaim.Name)
	call := p.computeService.Instances.AggregatedList(p.projectID).Filter(fmt.Sprintf("name eq %s", instanceName))

	regionPrefix := "zones/" + p.region + "-"
	var instance *compute.Instance
	err := call.Pages(ctx, func(resp *compute.InstanceAggregatedList) error {
		for zoneKey, items := range resp.Items {
			if !strings.HasPrefix(zoneKey, regionPrefix) {
				continue
			}
			for _, i := range items.Instances {
				instance = i
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return instance, nil
}

func (p *DefaultProvider) adoptExistingInstance(ctx context.Context, existingInstance *compute.Instance, capacityType string) *Instance {
	zone := existingInstance.Zone
	if split := strings.Split(zone, "/"); len(split) > 0 {
		zone = split[len(split)-1]
	}
	machineType := existingInstance.MachineType
	if split := strings.Split(machineType, "/"); len(split) > 0 {
		machineType = split[len(split)-1]
	}

	log.FromContext(ctx).Info("Found existing instance for NodeClaim", "instance", existingInstance.Name, "zone", zone)
	return &Instance{
		InstanceID:   existingInstance.Name,
		Name:         existingInstance.Name,
		Type:         machineType,
		Location:     zone,
		ProjectID:    p.projectID,
		ImageID:      resolveInstanceImage(existingInstance),
		CreationTime: parseCreationTime(existingInstance.CreationTimestamp),
		CapacityType: capacityType,
		Labels:       existingInstance.Labels,
		Tags:         existingInstance.Labels,
		Status:       existingInstance.Status,
	}
}

func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error) {
	if len(instanceTypes) == 0 {
		return nil, fmt.Errorf("no instance types provided")
	}

	instanceTypes = orderInstanceTypesByPrice(instanceTypes, scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...))
	capacityType := p.getCapacityType(nodeClaim, instanceTypes)

	// Check if the instance already exists in any zone
	if existingInstance, err := p.findInstanceByNodeClaim(ctx, nodeClaim); err != nil {
		log.FromContext(ctx).Error(err, "failed to check if instance exists in region", "nodeClaim", nodeClaim.Name)
	} else if existingInstance != nil {
		return p.adoptExistingInstance(ctx, existingInstance, capacityType), nil
	}

	var errs []error
	// try all instance types, if one is available, use it
	for _, instanceType := range instanceTypes {
		instance, zone, template, err := p.tryCreateInstance(ctx, nodeClass, nodeClaim, instanceType, capacityType)
		if err != nil {
			var retryableErr *retryableError
			if errors.As(err, &retryableErr) {
				errs = append(errs, retryableErr.err)
				continue
			}
			return nil, err
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
			ImageID:      resolveInstanceImage(instance),
			CreationTime: time.Now(),
			CapacityType: capacityType,
			Tags:         template.Properties.Labels,
			Labels:       instance.Labels,
			Status:       InstanceStatusProvisioning,
		}, nil
	}

	if len(errs) == 0 {
		return nil, fmt.Errorf("failed to create instance after trying all instance types: unknown error")
	}

	joined := errors.Join(errs...)
	if lo.SomeBy(errs, func(err error) bool { return cloudprovider.IsInsufficientCapacityError(err) }) {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("failed to create instance after trying all instance types: %w", joined))
	}

	return nil, fmt.Errorf("failed to create instance after trying all instance types: %w", joined)
}

type retryableError struct {
	err error
}

func (e *retryableError) Error() string {
	return e.err.Error()
}

func (p *DefaultProvider) tryCreateInstance(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim, instanceType *cloudprovider.InstanceType, capacityType string) (*compute.Instance, string, *compute.InstanceTemplate, error) {
	zone, err := p.selectZone(ctx, nodeClaim, instanceType, capacityType)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to select zone for instance type", "instanceType", instanceType.Name)
		return nil, "", nil, &retryableError{err}
	}

	imageFamily := nodeClass.ImageFamily()
	arch := instanceType.Requirements.Get(corev1.LabelArchStable).Any()
	if arch == "" {
		arch = imagefamily.OSArchAMD64Requirement
	}
	nodePoolName := resolveNodePoolName(imageFamily, arch)
	if nodePoolName == "" {
		err := fmt.Errorf("failed to resolve node pool name for image family %q", imageFamily)
		log.FromContext(ctx).Error(err, "failed to resolve node pool name for image family", "imageFamily", imageFamily)
		return nil, "", nil, err
	}

	template, err := p.findTemplateByNodePoolName(ctx, nodePoolName)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to find template for alias", "alias", nodeClass.Spec.ImageSelectorTerms[0].Alias)
		return nil, "", nil, &retryableError{err}
	}

	clusterConfig, err := p.gkeProvider.GetClusterConfig(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to fetch cluster config")
		return nil, "", nil, &retryableError{err}
	}

	instance, retryable, err := p.getOrCreateInstance(ctx, nodeClaim, nodeClass, instanceType, template, clusterConfig, nodePoolName, zone, capacityType)
	if err != nil {
		if retryable {
			return nil, "", nil, &retryableError{err}
		}
		return nil, "", nil, err
	}

	return instance, zone, template, nil
}

func (p *DefaultProvider) getOrCreateInstance(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, template *compute.InstanceTemplate, clusterConfig *container.Cluster, nodePoolName, zone, capacityType string) (*compute.Instance, bool, error) {
	instanceName := fmt.Sprintf("karpenter-%s", nodeClaim.Name)
	instance, exists, err := p.isInstanceExists(ctx, zone, instanceName)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to check if instance exists", "instanceName", instanceName)
		return nil, false, fmt.Errorf("failed to check if instance exists: %w", err)
	}

	if exists {
		return instance, false, nil
	}

	instance, err = p.buildInstance(nodeClaim, nodeClass, instanceType, template, clusterConfig, nodePoolName, zone, instanceName, capacityType)
	if err != nil {
		return nil, false, fmt.Errorf("building instance %s: %w", instanceName, err)
	}
	op, err := p.computeService.Instances.Insert(p.projectID, zone, instance).Context(ctx).Do()
	if err != nil {
		reason, reasonCode, insufficient := extractInsertInsufficientCapacityReason(err)
		if insufficient {
			if reason == "" {
				reason = "insufficient capacity"
			}
			ttl := insufficientCapacityBackoffTTL(reasonCode)
			p.unavailableOfferings.MarkUnavailableWithTTL(ctx, reason, instanceType.Name, zone, capacityType, ttl)
			err = cloudprovider.NewInsufficientCapacityError(fmt.Errorf("zone %s insufficient capacity: %s", zone, reason))

			// If IP space is exhausted, trying other instance types won't help as they share the same subnet.
			// We should fail fast to avoid unnecessary API calls and noise.
			if reasonCode == "IP_SPACE_EXHAUSTED" || reasonCode == "IP_SPACE_EXHAUSTED_WITH_DETAILS" {
				return nil, false, err
			}
		}
		log.FromContext(ctx).Error(err, "failed to create instance", "instanceType", instanceType.Name, "zone", zone)
		return nil, true, err
	}

	if err := p.waitOperationDone(ctx, instanceType.Name, zone, capacityType, op.Name); err != nil {
		log.FromContext(ctx).Error(err, "failed to wait for operation to be done", "instanceType", instanceType.Name, "zone", zone)
		return nil, true, err
	}

	return instance, false, nil
}

func resolveInstanceImage(instance *compute.Instance) string {
	image, ok := lo.Find(instance.Disks, func(disk *compute.AttachedDisk) bool {
		return disk.Boot
	})
	if !ok {
		return ""
	}
	if image.InitializeParams == nil {
		return ""
	}
	return image.InitializeParams.SourceImage
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

func filterZonesByRequirement(zones []string, reqs scheduling.Requirements) []string {
	zoneReq := reqs.Get(corev1.LabelTopologyZone)
	if zoneReq == nil || len(zoneReq.Values()) == 0 {
		return zones
	}
	allowed := sets.NewString()
	for _, z := range zones {
		if lo.Contains(zoneReq.Values(), z) {
			allowed.Insert(z)
		}
	}
	return allowed.List()
}

func randomZone(zones []string) string {
	randomIndex, _ := rand.Int(rand.Reader, big.NewInt(int64(len(zones))))
	return zones[randomIndex.Int64()]
}

func cheapestCompatibleZone(zones []string, reqs scheduling.Requirements, offerings cloudprovider.Offerings) string {
	cheapestZone := ""
	cheapestPrice := math.MaxFloat64
	zonesSet := sets.NewString(zones...)
	for _, offering := range offerings {
		if reqs.Compatible(offering.Requirements, scheduling.AllowUndefinedWellKnownLabels) != nil {
			continue
		}
		zone := offering.Requirements.Get(corev1.LabelTopologyZone).Any()
		if !zonesSet.Has(zone) {
			continue
		}
		if offering.Price < cheapestPrice {
			cheapestZone = zone
			cheapestPrice = offering.Price
		}
	}
	return cheapestZone
}

func (p *DefaultProvider) selectZone(ctx context.Context, nodeClaim *karpv1.NodeClaim,
	instanceType *cloudprovider.InstanceType, capacityType string,
) (string, error) {
	zones, err := p.gkeProvider.ResolveClusterZones(ctx)
	if err != nil {
		return "", err
	}

	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	zones = filterZonesByRequirement(zones, reqs)

	// If no zones remain after applying requirements, fail fast.
	if len(zones) == 0 {
		return "", fmt.Errorf("no zones match topology requirement %q", corev1.LabelTopologyZone)
	}

	// Skip zones with known ICE for this instance type and capacity type.
	// Without this filter, every retry calls MarkUnavailable and resets the
	// 30-min TTL, preventing natural expiry. Applied to both on-demand and
	// spot so both paths behave consistently.
	zones = lo.Filter(zones, func(z string, _ int) bool {
		return !p.unavailableOfferings.IsUnavailable(instanceType.Name, z, capacityType)
	})
	if len(zones) == 0 {
		return "", cloudprovider.NewInsufficientCapacityError(
			fmt.Errorf("all zones exhausted for instance type %s", instanceType.Name))
	}

	// For on-demand, randomly select a zone from those that satisfy the requirement,
	// to spread load while still honoring topology constraints.
	if capacityType == karpv1.CapacityTypeOnDemand {
		return randomZone(zones), nil
	}
	// else for spot, choose the cheapest zone
	return cheapestCompatibleZone(zones, reqs, instanceType.Offerings), nil
}

func resolveNodePoolName(imageFamily, arch string) string {
	switch imageFamily {
	case v1alpha1.ImageFamilyContainerOptimizedOS:
		if arch == imagefamily.OSArchARM64Requirement {
			return nodepooltemplate.KarpenterCOSARM64NodePoolTemplate
		}
		return nodepooltemplate.KarpenterDefaultNodePoolTemplate
	case v1alpha1.ImageFamilyUbuntu:
		if arch == imagefamily.OSArchARM64Requirement {
			return nodepooltemplate.KarpenterUbuntuARM64NodePoolTemplate
		}
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
	nodeClass *v1alpha1.GCENodeClass, zone string,
) ([]*compute.AttachedDisk, error) {
	disks := nodeClass.Spec.Disks
	sort.Slice(disks, func(i, j int) bool {
		return disks[i].Boot
	})

	attachedDisks := make([]*compute.AttachedDisk, len(disks))
	for i, disk := range disks {
		// Create a new disk configuration for each disk to avoid sharing references
		initParams := &compute.AttachedDiskInitializeParams{
			DiskType:   fmt.Sprintf("projects/%s/zones/%s/diskTypes/%s", p.projectID, zone, disk.Category),
			DiskSizeGb: int64(disk.SizeGiB),
		}
		if disk.ProvisionedIOPS != nil {
			initParams.ProvisionedIops = *disk.ProvisionedIOPS
		}
		if disk.ProvisionedThroughput != nil {
			initParams.ProvisionedThroughput = *disk.ProvisionedThroughput
		}
		attachedDisk := &compute.AttachedDisk{
			AutoDelete:       true,
			Boot:             disk.Boot,
			InitializeParams: initParams,
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
		} else {
			attachedDisk.DeviceName = metadata.GetSecondaryDiskImageDeviceName(disk.SecondaryBootImage)
			attachedDisk.InitializeParams.SourceImage = disk.SecondaryBootImage
		}

		// Add disk encryption key if specified
		if disk.KMSKeyName != "" {
			attachedDisk.DiskEncryptionKey = &compute.CustomerEncryptionKey{
				KmsKeyName:           disk.KMSKeyName,
				KmsKeyServiceAccount: disk.KMSKeyServiceAccount,
			}
		}

		attachedDisks[i] = attachedDisk
	}

	return attachedDisks, nil
}

func (p *DefaultProvider) buildInstance(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, template *compute.InstanceTemplate, clusterConfig *container.Cluster, nodePoolName, zone, instanceName, capacityType string) (*compute.Instance, error) {
	attachedDisks, err := p.renderDiskProperties(instanceType, nodeClass, zone)
	if err != nil {
		return nil, fmt.Errorf("rendering disk properties: %w", err)
	}

	// Setup metadata
	if err := p.setupInstanceMetadata(template.Properties.Metadata, nodeClass, instanceType, nodeClaim, nodePoolName, capacityType); err != nil {
		return nil, fmt.Errorf("setting up instance metadata: %w", err)
	}

	serviceAccounts, err := p.setupServiceAccounts(nodeClass)
	if err != nil {
		return nil, fmt.Errorf("setting up service accounts: %w", err)
	}

	instance := &compute.Instance{
		Name:              instanceName,
		MachineType:       fmt.Sprintf("zones/%s/machineTypes/%s", zone, instanceType.Name),
		Disks:             attachedDisks,
		NetworkInterfaces: p.setupNetworkInterfaces(clusterConfig, nodeClass),
		ServiceAccounts:   serviceAccounts,
		Metadata:          template.Properties.Metadata,
		Labels:            p.initializeInstanceLabels(nodeClass),
		Scheduling:        setupScheduling(capacityType),
		Tags:              buildInstanceTags(clusterConfig.Name, nodeClass.Spec.NetworkTags),
	}

	// Configure Shielded VM options
	if nodeClass.Spec.ShieldedInstanceConfig != nil {
		sic := nodeClass.Spec.ShieldedInstanceConfig
		instance.ShieldedInstanceConfig = &compute.ShieldedInstanceConfig{}
		if sic.EnableSecureBoot != nil {
			instance.ShieldedInstanceConfig.EnableSecureBoot = *sic.EnableSecureBoot
			instance.ShieldedInstanceConfig.ForceSendFields = append(instance.ShieldedInstanceConfig.ForceSendFields, "EnableSecureBoot")
		}
		if sic.EnableVtpm != nil {
			instance.ShieldedInstanceConfig.EnableVtpm = *sic.EnableVtpm
			instance.ShieldedInstanceConfig.ForceSendFields = append(instance.ShieldedInstanceConfig.ForceSendFields, "EnableVtpm")
		}
		if sic.EnableIntegrityMonitoring != nil {
			instance.ShieldedInstanceConfig.EnableIntegrityMonitoring = *sic.EnableIntegrityMonitoring
			instance.ShieldedInstanceConfig.ForceSendFields = append(instance.ShieldedInstanceConfig.ForceSendFields, "EnableIntegrityMonitoring")
		}
	}

	// Configure capacity provision
	p.configureInstanceCapacityProvision(instance, capacityType)

	// A2, A3, G2 machine types have built-in GPUs and do not support live migration.
	if instanceType.Requirements.Get(v1alpha1.LabelInstanceGPUCount).Len() > 0 {
		instance.Scheduling.OnHostMaintenance = "TERMINATE"
	}

	// Setup karpenter built-in labels
	p.setupInstanceLabels(instance, nodeClaim, nodeClass, instanceType)

	return instance, nil
}

func (p *DefaultProvider) setupNetworkInterfaces(cluster *container.Cluster, nodeClass *v1alpha1.GCENodeClass) []*compute.NetworkInterface {
	if cluster.NetworkConfig == nil {
		return nil
	}
	targetRange := podCIDRRange(nodeClass.GetMaxPods())
	clusterPrivate := cluster.PrivateClusterConfig != nil && cluster.PrivateClusterConfig.EnablePrivateNodes

	// Pod CIDR range name: NodeClass → cluster IpAllocationPolicy → let GKE pick.
	rangeName := ""
	if nodeClass.Spec.SubnetRangeName != nil {
		rangeName = *nodeClass.Spec.SubnetRangeName
	} else if cluster.IpAllocationPolicy != nil {
		rangeName = cluster.IpAllocationPolicy.ClusterSecondaryRangeName
	}

	// Primary interface: built from cluster config, overrideable via NodeClass networkConfig.
	subnetwork := cluster.NetworkConfig.Subnetwork
	disableExternal := clusterPrivate
	if nodeClass.Spec.NetworkConfig != nil {
		cfg := nodeClass.Spec.NetworkConfig
		if cfg.Subnetwork != "" {
			subnetwork = cfg.Subnetwork
		}
		if cfg.EnablePrivateNodes != nil {
			disableExternal = *cfg.EnablePrivateNodes
		}
	}

	primary := &compute.NetworkInterface{
		Network:    cluster.NetworkConfig.Network,
		Subnetwork: subnetwork,
		AliasIpRanges: []*compute.AliasIpRange{{
			IpCidrRange:         fmt.Sprintf("/%d", targetRange),
			SubnetworkRangeName: rangeName,
		}},
	}
	applyAccessConfig(primary, disableExternal)

	ifaces := []*compute.NetworkInterface{primary}

	if nodeClass.Spec.NetworkConfig != nil {
		ifaces = append(ifaces, buildAdditionalInterfaces(cluster.NetworkConfig.Network, nodeClass.Spec.NetworkConfig.AdditionalNetworkInterfaces, disableExternal)...)
	}

	return ifaces
}

// applyAccessConfig sets the ONE_TO_ONE_NAT access config on iface when external is enabled,
// or force-sends an empty slice so the GCP API does not insert it automatically.
func applyAccessConfig(iface *compute.NetworkInterface, disableExternal bool) {
	if !disableExternal {
		iface.AccessConfigs = []*compute.AccessConfig{{Type: "ONE_TO_ONE_NAT", Name: "External NAT"}}
	} else {
		iface.ForceSendFields = []string{"AccessConfigs"}
	}
}

// buildAdditionalInterfaces builds secondary NetworkInterface objects from additionalNetworkInterfaces.
// Subnetwork is required by the CRD schema on each entry. Network falls back to the cluster network
// when not explicitly set, mirroring GKE's additional_node_network_configs behavior.
func buildAdditionalInterfaces(clusterNetwork string, overrides []v1alpha1.AdditionalNetworkInterface, disableExternal bool) []*compute.NetworkInterface {
	ifaces := make([]*compute.NetworkInterface, 0, len(overrides))
	for _, override := range overrides {
		network := clusterNetwork
		if override.Network != "" {
			network = override.Network
		}
		iface := &compute.NetworkInterface{
			Network:    network,
			Subnetwork: override.Subnetwork,
		}
		applyAccessConfig(iface, disableExternal)
		ifaces = append(ifaces, iface)
	}
	return ifaces
}

// podCIDRRange returns the /N alias IP range size for the given maxPods count,
// following https://cloud.google.com/kubernetes-engine/docs/how-to/flexible-pod-cidr
func podCIDRRange(maxPods int32) int32 {
	switch {
	case maxPods <= 8:
		return 28
	case maxPods <= 16:
		return 27
	case maxPods <= 32:
		return 26
	case maxPods <= 64:
		return 25
	case maxPods <= 128:
		return 24
	default:
		return int32(maxNodeCIDR)
	}
}

// setupInstanceMetadata configures all metadata-related settings for the instance
func (p *DefaultProvider) setupInstanceMetadata(instanceMetadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, nodeClaim *karpv1.NodeClaim, nodePoolName string, capacityType string) error {
	if err := metadata.RemoveGKEBuiltinLabels(instanceMetadata, nodePoolName); err != nil {
		return fmt.Errorf("failed to remove GKE builtin labels from metadata: %w", err)
	}

	if err := metadata.SetMaxPodsPerNode(instanceMetadata, nodeClass); err != nil {
		return fmt.Errorf("failed to set max pods per node in metadata: %w", err)
	}

	if err := metadata.RenderKubeletConfigMetadata(instanceMetadata, instanceType, capacityType); err != nil {
		return fmt.Errorf("failed to render kubelet config metadata: %w", err)
	}

	if err := metadata.PatchUnregisteredTaints(instanceMetadata); err != nil {
		return fmt.Errorf("failed to append unregistered taint to kube-env: %w", err)
	}

	if err := metadata.PatchKubeEnvForInstanceType(instanceMetadata, instanceType); err != nil {
		return fmt.Errorf("failed to patch kube-env for instance type: %w", err)
	}

	if capacityType == karpv1.CapacityTypeSpot {
		if err := metadata.SetProvisioningModel(instanceMetadata, capacityType); err != nil {
			return fmt.Errorf("failed to set provisioning model in metadata: %w", err)
		}
	}

	metadata.AppendNodeClaimLabel(nodeClaim, nodeClass, instanceMetadata)
	metadata.AppendRegisteredLabel(instanceMetadata)
	metadata.AppendSecondaryBootDisks(p.projectID, nodeClass, instanceMetadata)
	metadata.ApplyCustomMetadata(instanceMetadata, nodeClass.Spec.Metadata)

	return nil
}

// setupServiceAccounts configures service accounts for the instance.
// Returns an error if no service account can be resolved, which means the project has
// disabled the Compute Engine default SA and neither spec.serviceAccount nor
// DEFAULT_NODEPOOL_SERVICE_ACCOUNT is set — nodes would fail to authenticate without an SA.
func (p *DefaultProvider) setupServiceAccounts(nodeClass *v1alpha1.GCENodeClass) ([]*compute.ServiceAccount, error) {
	serviceAccount := p.resolveServiceAccount(nodeClass)
	if serviceAccount == "" {
		return nil, fmt.Errorf("no service account available: spec.serviceAccount and DEFAULT_NODEPOOL_SERVICE_ACCOUNT are unset, and the Compute Engine default SA is unavailable for this project")
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
	}, nil
}

// setupScheduling returns scheduling config derived from capacity type alone.
// Spot-specific fields (provisioning model, preemptibility) are set later by
// configureInstanceCapacityProvision; this only wires the termination action so
// GCE honors DELETE rather than the default STOP on preemption.
func setupScheduling(capacityType string) *compute.Scheduling {
	sched := &compute.Scheduling{}
	if capacityType == karpv1.CapacityTypeSpot {
		sched.InstanceTerminationAction = instanceTerminationActionDelete
	}
	return sched
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

// setupInstanceLabels writes all GCE labels for a new instance. Cluster-identity labels
// (goog-k8s-cluster-name and goog-k8s-cluster-location) are always written last so that
// user-supplied NodePool requirement labels with the same key cannot overwrite them.
func (p *DefaultProvider) setupInstanceLabels(instance *compute.Instance, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType) {
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelNodePoolKey)] = nodeClaim.Labels[karpv1.NodePoolLabelKey]
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey)] = nodeClass.Name

	for key, req := range instanceType.Requirements {
		if req.Operator() == corev1.NodeSelectorOpIn && req.Len() == 1 {
			// GCP label keys only allow [a-z0-9_-]; Kubernetes label keys may contain '/' and '.'.
			instance.Labels[utils.SanitizeGCELabelValue(key)] = utils.SanitizeGCELabelValue(req.Any())
		}
	}

	// Stamp both cluster-identity labels last so NodePool labels cannot overwrite them.
	// LabelClusterNameKey is used by the syncInstances API filter; LabelClusterLocationKey
	// is checked by belongsToCluster to distinguish same-named clusters in different locations.
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterNameKey)] = p.clusterName
	instance.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)] = p.clusterLocation
}

// belongsToCluster reports whether inst should be tracked in this controller's instance
// cache. An instance with goog-k8s-cluster-location present must match this cluster's
// location. An instance without the label (created by an older Karpenter version) is
// treated as ours for backward compatibility — the GC controller separately skips
// label-less instances to prevent cross-cluster deletion. Cluster-name is enforced by the
// GCE API label filter in syncInstances.
//
// TODO: once all instances carry goog-k8s-cluster-location (i.e. after all clusters have
// been upgraded past this release and all pre-existing nodes have been rotated), replace
// the !ok fallback with strict matching (ok && loc == p.clusterLocation) and add the
// label as a server-side filter in syncInstances. Until then the fallback must stay to
// avoid making pre-label instances invisible to Karpenter.
func (p *DefaultProvider) belongsToCluster(inst *Instance) bool {
	loc, ok := inst.Labels[utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)]
	return !ok || loc == p.clusterLocation
}

// buildInstanceTags constructs GCE network tags for a new node.
// The cluster-wide tag (gke-<clusterName>-node) is always included — GKE's
// cluster-level firewall rules match on this tag. NodeClass network tags are
// appended after so callers can extend without replacing cluster routing.
func buildInstanceTags(clusterName string, networkTags []v1alpha1.NetworkTag) *compute.Tags {
	items := make([]string, 0, 1+len(networkTags))
	items = append(items, fmt.Sprintf("gke-%s-node", clusterName))
	for _, t := range networkTags {
		items = append(items, string(t))
	}
	return &compute.Tags{Items: lo.Uniq(items)}
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
	project, zone, instanceName, err := parseGCEProviderID(providerID)
	if err != nil {
		return fmt.Errorf("parsing provider ID: %w", err)
	}

	log.FromContext(ctx).Info("Deleting instance", "project", project, "zone", zone, "instance", instanceName)

	// Check current state before issuing a delete. This prevents sending multiple overlapping
	// delete operations to GCE (which cause 403 rateLimitExceeded on repeated reconciles).
	inst, err := p.computeService.Instances.Get(project, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			log.FromContext(ctx).Info("Instance already deleted", "instance", instanceName)
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance not found: %w", err))
		}
		return fmt.Errorf("getting instance state before delete: %w", err)
	}

	switch inst.Status {
	case InstanceStatusTerminated:
		log.FromContext(ctx).Info("Instance already terminated", "instance", instanceName)
		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("instance %s is already %s", instanceName, inst.Status))
	case InstanceStatusStopping:
		// Delete already in flight; caller re-queues and rechecks.
		log.FromContext(ctx).Info("Instance delete already in progress", "instance", instanceName)
		return nil
	}

	op, err := p.computeService.Instances.Delete(project, zone, instanceName).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			log.FromContext(ctx).Info("Instance disappeared before delete call", "instance", instanceName)
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

	for _, inst := range instances {
		if p.belongsToCluster(inst) {
			p.instanceCache.Set(inst.InstanceID, inst, cache.DefaultExpiration)
		}
	}
	return nil
}

// resolveServiceAccount returns the service account email for the instance.
// Priority: spec.serviceAccount → DEFAULT_NODEPOOL_SERVICE_ACCOUNT → Compute Engine default SA.
func (p *DefaultProvider) resolveServiceAccount(nodeClass *v1alpha1.GCENodeClass) string {
	if nodeClass.Spec.ServiceAccount != "" {
		return nodeClass.Spec.ServiceAccount
	}
	if p.defaultServiceAccount != "" {
		return p.defaultServiceAccount
	}
	return p.computeDefaultSA
}
