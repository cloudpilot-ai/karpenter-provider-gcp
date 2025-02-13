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
	"github.com/cloudpilot-ai/karpenter-provider-alibabacloud/pkg/utils"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	cloudproviderevents "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cloudprovider/events"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
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
		kubeClient: kubeClient,
		recorder:   recorder,

		instanceTypeProvider: instanceTypeProvider,
		instanceProvider:     instanceProvider,
	}
}

// Create a NodeClaim given the constraints.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	nodeClass, err := c.resolveNodeClassFromNodeClaim(ctx, nodeClaim)
	if err != nil {
		if errors.IsNotFound(err) {
			c.recorder.Publish(cloudproviderevents.NodeClaimFailedToResolveNodeClass(nodeClaim))
		}
		// We treat a failure to resolve the NodeClass as an ICE since this means there is no capacity possibilities for this NodeClaim
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("resolving node class, %w", err))
	}

	nodeClassReady := nodeClass.StatusConditions().Get(status.ConditionReady)
	if nodeClassReady.IsFalse() {
		return nil, cloudprovider.NewNodeClassNotReadyError(stderrors.New(nodeClassReady.Message))
	}
	if nodeClassReady.IsUnknown() {
		return nil, fmt.Errorf("resolving NodeClass readiness, NodeClass is in Ready=Unknown, %s", nodeClassReady.Message)
	}
	instanceTypes, err := c.resolveInstanceTypes(ctx, nodeClaim, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("resolving instance types, %w", err)
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
	instances, err := c.instanceProvider.List(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing instances")
		return nil, fmt.Errorf("listing instances, %w", err)
	}
	var nodeClaims []*karpv1.NodeClaim
	for i := range instances {
		instanceType, err := c.resolveInstanceTypeFromInstance(ctx, instances[i])
		if err != nil {
			log.FromContext(ctx).Error(err, "resolving instance type")
			return nil, fmt.Errorf("resolving instance type, %w", err)
		}
		nc, err := c.resolveNodeClassFromInstance(ctx, instances[i])
		if client.IgnoreNotFound(err) != nil {
			log.FromContext(ctx).Error(err, "resolving nodeclass")
			return nil, fmt.Errorf("resolving nodeclass, %w", err)
		}
		nodeClaims = append(nodeClaims, c.instanceToNodeClaim(instances[i], instanceType, nc))
	}
	return nodeClaims, nil
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	id, err := utils.ParseInstanceID(providerID)
	if err != nil {
		log.FromContext(ctx).Error(err, "parsing instance ID")
		return nil, fmt.Errorf("getting instance ID, %w", err)
	}

	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("id", id))

	instance, err := c.instanceProvider.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting instance, %w", err)
	}

	instanceType, err := c.resolveInstanceTypeFromInstance(ctx, instance)
	if err != nil {
		log.FromContext(ctx).Error(err, "resolving instance type")
		return nil, fmt.Errorf("resolving instance type, %w", err)
	}

	nc, err := c.resolveNodeClassFromInstance(ctx, instance)
	if client.IgnoreNotFound(err) != nil {
		log.FromContext(ctx).Error(err, "resolving nodeclass")
		return nil, fmt.Errorf("resolving nodeclass, %w", err)
	}

	return c.instanceToNodeClaim(instance, instanceType, nc), nil
}

func (c *CloudProvider) LivenessProbe(req *http.Request) error {
	return c.instanceTypeProvider.LivenessProbe(req)
}

// GetInstanceTypes returns all available InstanceTypes
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		if errors.IsNotFound(err) {
			c.recorder.Publish(cloudproviderevents.NodePoolFailedToResolveNodeClass(nodePool))
		}
		// We must return an error here in the event of the node class not being found. Otherwise, users just get
		// no instance types and a failure to schedule with no indicator pointing to a bad configuration
		// as the cause.
		log.FromContext(ctx).Error(err, "resolving node class")
		return nil, err
	}
	// TODO: break this coupling
	instanceTypes, err := c.instanceTypeProvider.List(ctx, nodeClass.Spec.KubeletConfiguration, nodeClass)
	if err != nil {
		log.FromContext(ctx).Error(err, "listing instance types")
		return nil, err
	}
	return instanceTypes, nil
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	id, err := utils.ParseInstanceID(nodeClaim.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("getting instance ID, %w", err)
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("id", id))
	return c.instanceProvider.Delete(ctx, id)
}

func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	// Not needed when GetInstanceTypes removes nodepool dependency
	nodePoolName, ok := nodeClaim.Labels[karpv1.NodePoolLabelKey]
	if !ok {
		return "", nil
	}
	nodePool := &karpv1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return "", nil
	}
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		if errors.IsNotFound(err) {
			c.recorder.Publish(cloudproviderevents.NodePoolFailedToResolveNodeClass(nodePool))
		}
		return "", client.IgnoreNotFound(fmt.Errorf("resolving node class, %w", err))
	}
	driftReason, err := c.isNodeClassDrifted(ctx, nodeClaim, nodePool, nodeClass)
	if err != nil {
		return "", err
	}
	return driftReason, nil
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

func (c *CloudProvider) resolveInstanceTypeFromInstance(ctx context.Context, instance *instance.Instance) (*cloudprovider.InstanceType, error) {
	nodePool, err := c.resolveNodePoolFromInstance(ctx, instance)
	if err != nil {
		// If we can't resolve the NodePool, we fall back to not getting instance type info
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving nodepool, %w", err))
	}
	instanceTypes, err := c.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		// If we can't resolve the NodePool, we fall back to not getting instance type info
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving nodeclass, %w", err))
	}
	instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
		return i.Name == instance.Type
	})
	return instanceType, nil
}

func (c *CloudProvider) resolveNodePoolFromInstance(ctx context.Context, instance *instance.Instance) (*karpv1.NodePool, error) {
	if nodePoolName, ok := instance.Tags[karpv1.NodePoolLabelKey]; ok {
		nodePool := &karpv1.NodePool{}
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
			return nil, err
		}
		return nodePool, nil
	}
	return nil, errors.NewNotFound(schema.GroupResource{Group: coreapis.Group, Resource: "nodepools"}, "")
}

func (c *CloudProvider) resolveNodeClassFromInstance(ctx context.Context, instance *instance.Instance) (*v1alpha1.GCENodeClass, error) {
	np, err := c.resolveNodePoolFromInstance(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("resolving nodepool, %w", err)
	}
	return c.resolveNodeClassFromNodePool(ctx, np)
}

func (c *CloudProvider) resolveNodeClassFromNodePool(ctx context.Context, nodePool *karpv1.NodePool) (*v1alpha1.GCENodeClass, error) {
	nodeClass := &v1alpha1.GCENodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, err
	}
	if !nodeClass.DeletionTimestamp.IsZero() {
		// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound,
		// but we return a different error message to be clearer to users
		return nil, newTerminatingNodeClassError(nodeClass.Name)
	}
	return nodeClass, nil
}

// newTerminatingNodeClassError returns a NotFound error for handling by
func newTerminatingNodeClassError(name string) *errors.StatusError {
	qualifiedResource := schema.GroupResource{Group: apis.Group, Resource: "GCEnodeclasses"}
	err := errors.NewNotFound(qualifiedResource, name)
	err.ErrStatus.Message = fmt.Sprintf("%s %q is terminating, treating as not found", qualifiedResource.String(), name)
	return err
}

func (c *CloudProvider) instanceToNodeClaim(i *instance.Instance, instanceType *cloudprovider.InstanceType, _ *v1alpha1.GCENodeClass) *karpv1.NodeClaim {
	nodeClaim := &karpv1.NodeClaim{}
	labels := map[string]string{}
	annotations := map[string]string{}

	if instanceType != nil {
		labels = utils.GetAllSingleValuedRequirementLabels(instanceType)
		resourceFilter := func(n corev1.ResourceName, v resource.Quantity) bool {
			return !resources.IsZero(v)
		}
		nodeClaim.Status.Capacity = lo.PickBy(instanceType.Capacity, resourceFilter)
		nodeClaim.Status.Allocatable = lo.PickBy(instanceType.Allocatable(), resourceFilter)
	}
	labels[corev1.LabelTopologyZone] = i.Zone
	labels[karpv1.CapacityTypeLabelKey] = i.CapacityType
	if v, ok := i.Tags[karpv1.NodePoolLabelKey]; ok {
		labels[karpv1.NodePoolLabelKey] = v
	}
	nodeClaim.Labels = labels
	nodeClaim.Annotations = annotations

	nodeClaim.CreationTimestamp = metav1.Time{Time: i.CreationTime}

	// If the instance is STOPPING or STOPPED, we set the deletion timestamp to now
	if i.Status == instance.InstanceStatusStopping || i.Status == instance.InstanceStatusStopped {
		nodeClaim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	}

	nodeClaim.Status.ProviderID = fmt.Sprintf("%s.%s", i.Region, i.ID)
	nodeClaim.Status.ImageID = i.ImageID
	return nodeClaim
}

func (c *CloudProvider) resolveNodeClassFromNodeClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*v1alpha1.GCENodeClass, error) {
	nodeClass := &v1alpha1.GCENodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClaim.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, err
	}
	// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound
	if !nodeClass.DeletionTimestamp.IsZero() {
		// For the purposes of NodeClass CloudProvider resolution, we treat deleting NodeClasses as NotFound,
		// but we return a different error message to be clearer to users
		return nil, newTerminatingNodeClassError(nodeClass.Name)
	}
	return nodeClass, nil
}

func (c *CloudProvider) resolveInstanceTypes(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error) {
	instanceTypes, err := c.instanceTypeProvider.List(ctx, nodeClass.Spec.KubeletConfiguration, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("getting instance types, %w", err)
	}
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	return lo.Filter(instanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		return reqs.Compatible(i.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil &&
			len(i.Offerings.Compatible(reqs).Available()) > 0 &&
			resources.Fits(nodeClaim.Spec.Resources.Requests, i.Allocatable())
	}), nil
}
