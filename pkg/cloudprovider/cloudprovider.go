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
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	cloudproviderevents "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cloudprovider/events"
	gcperrors "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/errors"
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
	nodePool, err := c.resolveNodePoolFromNodeClaim(ctx, nodeClaim)
	if err != nil {
		return nil, fmt.Errorf("resolving nodepool, %w", err)
	}
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

	instanceTypes, err := c.resolveInstanceTypes(ctx, nodeClaim, nodeClass)
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

	nc := c.instanceToNodeClaim(instance, instanceType)
	nc.Annotations = lo.Assign(nc.Annotations, map[string]string{
		v1alpha1.AnnotationGCENodeClassHash:        nodePool.Annotations[v1alpha1.AnnotationGCENodeClassHash],
		v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion,
	})
	return nc, nil
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	instances, err := c.instanceProvider.List(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to list instances")
		return nil, fmt.Errorf("listing instances, %w", err)
	}

	var nodeClaims []*karpv1.NodeClaim
	for _, instance := range instances {
		instanceType, err := c.resolveInstanceTypeFromInstance(ctx, instance)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to resolve instance type")
			return nil, fmt.Errorf("resolving instance type, %w", err)
		}

		nodeClaim := c.instanceToNodeClaim(instance, instanceType)
		nodeClaims = append(nodeClaims, nodeClaim)
	}

	log.FromContext(ctx).Info("listed all nodeclaims", "count", len(nodeClaims))
	return nodeClaims, nil
}

func (c *CloudProvider) resolveInstanceTypeFromInstance(ctx context.Context, instance *instance.Instance) (*cloudprovider.InstanceType, error) {
	nodePool, err := c.resolveNodePoolFromInstance(ctx, instance)
	if err != nil {
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving nodepool, %w", err))
	}

	instanceTypes, err := c.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		return nil, client.IgnoreNotFound(fmt.Errorf("getting instance types, %w", err))
	}

	instanceType, ok := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool {
		return i.Name == instance.Type
	})

	if !ok {
		return nil, fmt.Errorf("instance type %s not found in offerings", instance.Type)
	}
	return instanceType, nil
}

func (c *CloudProvider) resolveNodePoolFromInstance(ctx context.Context, instance *instance.Instance) (*karpv1.NodePool, error) {
	nodePoolName := instance.Labels[utils.SanitizeGCELabelValue(utils.LabelNodePoolKey)]
	if nodePoolName == "" {
		return nil, fmt.Errorf("missing nodepool label")
	}

	var nodePool karpv1.NodePool
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodePoolName}, &nodePool); err != nil {
		return nil, err
	}

	return &nodePool, nil
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("providerID", providerID))

	instance, err := c.instanceProvider.Get(ctx, providerID)
	if err != nil {
		return nil, fmt.Errorf("getting instance, %w", err)
	}

	instanceType, err := c.resolveInstanceTypeFromInstance(ctx, instance)
	if err != nil {
		log.FromContext(ctx).Error(err, "resolving instance type")
		return nil, fmt.Errorf("resolving instance type, %w", err)
	}

	return c.instanceToNodeClaim(instance, instanceType), nil
}

func (c *CloudProvider) LivenessProbe(req *http.Request) error {
	return c.instanceTypeProvider.LivenessProbe(req)
}

// GetInstanceTypes returns all available InstanceTypes
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		if errors.IsNotFound(err) {
			// If we can't resolve the NodeClass, then it's impossible for us to resolve the instance types
			c.recorder.Publish(cloudproviderevents.NodePoolFailedToResolveNodeClass(nodePool))
		}
		log.FromContext(ctx).Error(gcperrors.ErrResolveNodeClass, "nodePool", nodePool)
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
	providerID := nodeClaim.Status.ProviderID
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("id", providerID))
	return c.instanceProvider.Delete(ctx, providerID)
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
	// if the nodepooltemplate hash annotation is missing, the node is not drifted
	if _, ok := nodePool.Annotations[v1alpha1.AnnotationGCENodeClassHash]; !ok {
		return "", nil
	}
	// if the nodeclaim hash annotation is missing, the node is not drifted
	if _, ok := nodeClaim.Annotations[v1alpha1.AnnotationGCENodeClassHash]; !ok {
		return "", nil
	}
	if nodePool.Annotations[v1alpha1.AnnotationGCENodeClassHash] != nodeClaim.Annotations[v1alpha1.AnnotationGCENodeClassHash] {
		return "GCENodeClassHashDrifted", nil
	}
	return "", nil
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

func (c *CloudProvider) instanceToNodeClaim(i *instance.Instance, instanceType *cloudprovider.InstanceType) *karpv1.NodeClaim {
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
	// Add instance type label for gce nodeclaim
	labels[corev1.LabelInstanceTypeStable] = instanceType.Name

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

func (c *CloudProvider) resolveNodePoolFromNodeClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodePool, error) {
	nodePoolName, ok := nodeClaim.Labels[karpv1.NodePoolLabelKey]
	if !ok {
		return nil, fmt.Errorf("nodeClaim missing NodePoolLabelKey")
	}
	nodePool := &karpv1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return nil, err
	}
	return nodePool, nil
}

func (c *CloudProvider) resolveInstanceTypes(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass) ([]*cloudprovider.InstanceType, error) {
	instanceTypes, err := c.instanceTypeProvider.List(ctx, nodeClass)
	if err != nil {
		return nil, err
	}

	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	return lo.Filter(instanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		return reqs.Compatible(i.Requirements, scheduling.AllowUndefinedWellKnownLabels) == nil &&
			len(i.Offerings.Compatible(reqs).Available()) > 0 &&
			resources.Fits(nodeClaim.Spec.Resources.Requests, i.Allocatable())
	}), nil
}
