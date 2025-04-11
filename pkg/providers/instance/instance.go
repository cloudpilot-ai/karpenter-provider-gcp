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
	"fmt"
	"time"

	"google.golang.org/api/compute/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
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
	instanceType := instanceTypes[0]

	// Resolve zone from NodeClaim or offerings
	zone := nodeClaim.Labels["topology.kubernetes.io/zone"]

	// zone should be based on the offering, for now lets return static zone
	// if zone == "" {
	// 	for _, offering := range instanceType.Offerings {
	// 		zone = offering.Zone()
	// 		break
	// 	}
	// }
	if zone == "" {
		return nil, fmt.Errorf("failed to resolve zone for instance launch")
	}

	alias := nodeClass.Spec.ImageSelectorTerms[0].Alias
	if alias == "" {
		return nil, fmt.Errorf("alias not specified in ImageSelectorTerm")
	}

	var templateName string

	// probably case should not be hardcoded but read from some provider instead
	switch alias {
	case "ContainerOptimizedOS":
		templateName = nodepooltemplate.KarpenterDefaultNodePoolTemplate
	case "Ubuntu":
		templateName = nodepooltemplate.KarpenterUbuntuNodePoolTemplate
	default:
		return nil, fmt.Errorf("unsupported image alias %q", alias)
	}

	template, err := p.computeService.RegionInstanceTemplates.Get(p.projectID, p.region, templateName).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("getting instance template: %w", err)
	}

	instance := &compute.Instance{
		Name:              fmt.Sprintf("karpenter-%s", nodeClaim.Name),
		MachineType:       fmt.Sprintf("zones/%s/machineTypes/%s", zone, instanceType.Name),
		Disks:             template.Properties.Disks,
		NetworkInterfaces: template.Properties.NetworkInterfaces,
		ServiceAccounts:   template.Properties.ServiceAccounts,
		Metadata:          template.Properties.Metadata,
		Labels:            map[string]string{},
		Scheduling:        template.Properties.Scheduling,
		Tags:              template.Properties.Tags,
	}

	// Apply scheduling config for Spot capacity, for now lets do on demand only
	// like: capacityType := getCapacityType(nodeClaim, instanceTypes)
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

	// Set common Karpenter labels
	instance.Labels[karpv1.NodePoolLabelKey] = nodeClaim.Labels[karpv1.NodePoolLabelKey]
	instance.Labels["karpenter.k8s.gcp/gcenodeclass"] = nodeClass.Name

	op, err := p.computeService.Instances.Insert(p.projectID, zone, instance).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("creating instance: %w", err)
	}
	log.FromContext(ctx).Info("Created instance %s\n", op.Name)

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

// func getCapacityType(nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) string {
// 	requirements := cloudprovider.Requirements(nodeClaim.Spec.Requirements)
// 	if requirements.Get(karpv1.CapacityTypeLabelKey).Has(karpv1.CapacityTypeSpot) {
// 		return karpv1.CapacityTypeSpot
// 	}
// 	return karpv1.CapacityTypeOnDemand
// }

func (p *DefaultProvider) Get(ctx context.Context, providerID string) (*Instance, error) {
	// TODO: Implement me
	return nil, nil
}

func (p *DefaultProvider) List(ctx context.Context) ([]*Instance, error) {
	// TODO: Implement me
	return nil, nil
}

func (p *DefaultProvider) Delete(ctx context.Context, providerID string) error {
	// TODO: Implement me
	return nil
}

func (p *DefaultProvider) CreateTags(ctx context.Context, providerID string, tags map[string]string) error {
	// TODO: Implement me
	return nil
}
