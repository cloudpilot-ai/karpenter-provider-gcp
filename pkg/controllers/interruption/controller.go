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

package interruption

import (
	"context"
	"fmt"
	"time"

	computev1 "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/metadata"
	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	corev1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/utils/node"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	interruptionevents "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/interruption/events"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const (
	OperationTypePreempted = "compute.instances.preempted"

	NodeConditionReasonKubeletNotReady = "KubeletNotReady"
	NodeConditionMessageShuttingDown   = "node is shutting down"

	InterruptionReason = "interruption"
)

// Controller is an GCP interruption controller.
type Controller struct {
	kubeClient client.Client
	recorder   events.Recorder

	instanceProvider instance.Provider

	unavailableOfferingsCache *cache.UnavailableOfferings
	metadataClient            *metadata.Client
	zoneOperationClient       *computev1.ZoneOperationsClient
	credential                auth.Credential
}

func NewController(kubeClient client.Client,
	recorder events.Recorder, unavailableOfferingsCache *cache.UnavailableOfferings,
	metadataClient *metadata.Client,
	zoneOperationClient *computev1.ZoneOperationsClient,
	credential auth.Credential,
	instanceProvider instance.Provider,
) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		recorder:   recorder,

		unavailableOfferingsCache: unavailableOfferingsCache,
		metadataClient:            metadataClient,
		zoneOperationClient:       zoneOperationClient,
		credential:                credential,
		instanceProvider:          instanceProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	if err := c.handleStoppingSpotInstances(ctx); err != nil {
		return reconciler.Result{}, fmt.Errorf("handling stopped spot instances: %w", err)
	}

	// Will requeue after 1 second and try again
	return reconciler.Result{RequeueAfter: 1 * time.Second}, nil
}

func (c *Controller) handleStoppingSpotInstances(ctx context.Context) error {
	nodes := &corev1.NodeList{}
	if err := c.kubeClient.List(ctx, nodes, &client.ListOptions{}); err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	for i := range nodes.Items {
		currentNode := nodes.Items[i]
		if currentNode.Labels == nil || currentNode.Labels[utils.LabelNodePoolKey] == "" {
			continue
		}

		condition := node.GetCondition(&currentNode, corev1.NodeReady)
		if condition.Status != corev1.ConditionTrue && condition.Reason == NodeConditionReasonKubeletNotReady && condition.Message == NodeConditionMessageShuttingDown {
			if err := c.cleanNodeClaimByInstanceName(ctx, currentNode.Name, false); err != nil {
				return fmt.Errorf("cleaning node claim: %w", err)
			}
		}
	}

	return nil
}

func (c *Controller) cleanNodeClaimByInstanceName(ctx context.Context, instanceName string, markUnavailable bool) error {
	nodeClaim, err := c.getNodeClaimByNodeName(ctx, instanceName)
	if err != nil {
		return fmt.Errorf("getting node claim by node name: %w", err)
	}
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return nil
	}
	zone := nodeClaim.Labels[corev1.LabelTopologyZone]
	instanceType := nodeClaim.Labels[corev1.LabelInstanceTypeStable]
	if markUnavailable && zone != "" && instanceType != "" {
		c.unavailableOfferingsCache.MarkUnavailable(ctx, OperationTypePreempted, instanceType, zone, karpv1.CapacityTypeSpot)
	}

	if err := c.deleteNodeClaim(ctx, nodeClaim); err != nil {
		return fmt.Errorf("deleting node claim: %w", err)
	}

	return nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("interruption").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

// deleteNodeClaim removes the NodeClaim from the api-server
func (c *Controller) deleteNodeClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("deleting the node on interruption message, %w", err))
	}
	log.FromContext(ctx).Info("initiating delete from interruption message", "nodeClaim", nodeClaim.Name)
	c.recorder.Publish(interruptionevents.TerminatingOnInterruption(nodeClaim)...)
	metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
		metrics.ReasonLabel:       InterruptionReason,
		metrics.NodePoolLabel:     nodeClaim.Labels[karpv1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: nodeClaim.Labels[karpv1.CapacityTypeLabelKey],
	})
	return nil
}

func (c *Controller) getNodeClaimByNodeName(ctx context.Context, nodeName string) (*karpv1.NodeClaim, error) {
	nodeClaimList := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaimList); err != nil {
		return nil, err
	}

	for ni := range nodeClaimList.Items {
		if nodeClaimList.Items[ni].Status.NodeName == nodeName {
			return nodeClaimList.Items[ni].DeepCopy(), nil
		}
	}

	return nil, fmt.Errorf("no nodeclaim found for node %s", nodeName)
}
