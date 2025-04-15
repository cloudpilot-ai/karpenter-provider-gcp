/*
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

package cloudprovider

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	cloudproviderevents "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cloudprovider/events"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const CloudProviderName = "gcp"

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	kubeClient client.Client
	recorder   events.Recorder

	instanceTypeProvider instancetype.Provider
	instanceProvider     instance.Provider
}

func New(kubeClient client.Client,
	recorder events.Recorder,
	instanceTypeProvider instancetype.Provider,
	instanceProvider instance.Provider,
) *CloudProvider {
	return &CloudProvider{
		kubeClient:           kubeClient,
		recorder:             recorder,
		instanceTypeProvider: instanceTypeProvider,
		instanceProvider:     instanceProvider,
	}
}

// Create a NodeClaim given the constraints.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	log.FromContext(ctx).Info("creating nodes", "nodeClaim", nodeClaim)
	nodeClass, err := c.resolveNodeClassFromNodeClaim(ctx, nodeClaim)
	if err != nil {
		if errors.IsNotFound(err) {
			c.recorder.Publish(cloudproviderevents.NodeClaimFailedToResolveNodeClass(nodeClaim))
		}
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("resolving node class, %w", err))
	}

	nodeClassReady := nodeClass.StatusConditions().Get(status.ConditionReady)
	if nodeClassReady.IsFalse() {
		return nil, cloudprovider.NewNodeClassNotReadyError(stderrors.New(nodeClassReady.Message))
	}
	if nodeClassReady.IsUnknown() {
		return nil, cloudprovider.NewCreateError(fmt.Errorf("resolving NodeClass readiness, NodeClass is in Ready=Unknown, %s", nodeClassReady.Message), "NodeClassReadinessUnknown", "NodeClass is in Ready=Unknown")
	}

	instanceTypes, err := c.resolveInstanceTypes(ctx, nodeClass)
	if err != nil {
		return nil, cloudprovider.NewCreateError(fmt.Errorf("resolving instance types, %w", err), "InstanceTypeResolutionFailed", "Error resolving instance types")
	}
	if len(instanceTypes) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("all requested instance types were unavailable during launch"))
	}

	instance, err := c.instanceProvider.Create(ctx, nodeClass, nodeClaim, instanceTypes)
	if err != nil {
		return nil, fmt.Errorf("creating instance, %w", err)
	}

	instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
		return i.Name == instance.Type
	})

	nc := c.instanceToNodeClaim(instance, instanceType, nodeClass)
	nc.Annotations = lo.Assign(nc.Annotations, map[string]string{
		v1alpha1.AnnotationGCENodeClassHash:        nodeClass.Hash(),
		v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion,
	})
	return nc, nil
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	return nil, fmt.Errorf("not implemented")

}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	return nil, fmt.Errorf("not implemented")

}

func (c *CloudProvider) LivenessProbe(req *http.Request) error {
	return nil
}

// GetInstanceTypes returns all available InstanceTypes
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	logger := log.FromContext(ctx)
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		if errors.IsNotFound(err) {
			// If we can't resolve the NodeClass, then it's impossible for us to resolve the instance types
			// c.recorder.Publish(cloudproviderevents.NodePoolFailedToResolveNodeClass(nodePool))
			logger.Error(fmt.Errorf("failed to resolve node class"), "nodePool", nodePool)
			return nil, nil
		}
		return nil, fmt.Errorf("resolving node class, %w", err)
	}
	// TODO, break this coupling
	instanceTypes, err := c.instanceTypeProvider.List(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	return instanceTypes, nil
}

func (c *CloudProvider) resolveNodeClassFromNodePool(ctx context.Context, nodePool *karpv1.NodePool) (*v1alpha1.GCENodeClass, error) {
	nodeClass := &v1alpha1.GCENodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, err
	}
	if !nodeClass.DeletionTimestamp.IsZero() {
		// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound,
		// but we return a different error message to be clearer to users
		return nil, nil // return nil, newTerminatingNodeClassError(nodeClass.Name)
	}
	return nodeClass, nil
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	return fmt.Errorf("not implemented")
}

func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", fmt.Errorf("not implemented")
}

func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		// Supported Kubelet Node Conditions
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionUnknown,
			TolerationDuration: 30 * time.Minute,
		},
		// Support Node Monitoring Agent Conditions
		//
		{
			ConditionType:      "AcceleratedHardwareReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 10 * time.Minute,
		},
		{
			ConditionType:      "StorageReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "NetworkingReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "KernelReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "ContainerRuntimeReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
	}
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return CloudProviderName
}

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.GCENodeClass{}}
}

func (c *CloudProvider) instanceToNodeClaim(i *instance.Instance, instanceType *cloudprovider.InstanceType, nodeClass *v1alpha1.GCENodeClass) *karpv1.NodeClaim {
	nodeClaim := &karpv1.NodeClaim{}
	labels := map[string]string{}
	annotations := map[string]string{}

	if instanceType != nil {
		labels = utils.GetAllSingleValuedRequirementLabels(instanceType)

		resourceFilter := func(name corev1.ResourceName, value resource.Quantity) bool {
			return !resources.IsZero(value)
		}

		nodeClaim.Status.Capacity = lo.PickBy(instanceType.Capacity, resourceFilter)
		nodeClaim.Status.Allocatable = lo.PickBy(instanceType.Allocatable(), resourceFilter)
	}

	// Set core labels
	labels[corev1.LabelTopologyZone] = i.Location
	labels[karpv1.CapacityTypeLabelKey] = i.CapacityType

	// Add node pool label if present
	if v, ok := i.Labels[karpv1.NodePoolLabelKey]; ok {
		labels[karpv1.NodePoolLabelKey] = v
	}

	nodeClaim.Labels = labels
	nodeClaim.Annotations = annotations
	nodeClaim.CreationTimestamp = metav1.Time{Time: i.CreationTime}

	// Handle instance deletion status (e.g., TERMINATED or STOPPING in GCP)
	if i.Status == instance.InstanceStatusTerminated || i.Status == instance.InstanceStatusStopping {
		nodeClaim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	}

	// ProviderID format for GCE: gce://project/zone/instance-name
	nodeClaim.Status.ProviderID = fmt.Sprintf("gce://%s/%s/%s", i.ProjectID, i.Location, i.Name)
	nodeClaim.Status.ImageID = i.ImageID

	return nodeClaim
}

func (c *CloudProvider) resolveNodeClassFromNodeClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*v1alpha1.GCENodeClass, error) {
	ref := nodeClaim.Spec.NodeClassRef
	if ref == nil {
		return nil, fmt.Errorf("nodeClaim missing NodeClassRef")
	}
	nodeClass := &v1alpha1.GCENodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: ref.Name}, nodeClass); err != nil {
		return nil, err
	}
	return nodeClass, nil
}

func (c *CloudProvider) resolveInstanceTypes(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error) {
	return c.instanceTypeProvider.List(ctx, nodeClass)
}
