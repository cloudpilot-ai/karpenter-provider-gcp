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

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	compute "google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/utils/node"

	interruptionevents "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/interruption/events"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
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

	unavailableOfferingsCache *unavailableofferings.UnavailableOfferings

	computeService *compute.Service
	projectID      string
}

func NewController(kubeClient client.Client, recorder events.Recorder, unavailableOfferingsCache *unavailableofferings.UnavailableOfferings,
	computeService *compute.Service, projectID string) *Controller {
	return &Controller{
		kubeClient:                kubeClient,
		recorder:                  recorder,
		unavailableOfferingsCache: unavailableOfferingsCache,
		computeService:            computeService,
		projectID:                 projectID,
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
			if err := c.cleanNodeClaimByInstanceName(ctx, currentNode.Name); err != nil {
				return fmt.Errorf("cleaning node claim: %w", err)
			}
		}
	}

	return nil
}

// cleanNodeClaimByInstanceName deletes the NodeClaim backing the named node. When the node is
// a spot node, the (instanceType, zone) offering is first recorded as unavailable so that the
// scheduler does not immediately relaunch into the zone that just reclaimed capacity.
func (c *Controller) cleanNodeClaimByInstanceName(ctx context.Context, instanceName string) error {
	nodeClaim, err := c.getNodeClaimByNodeName(ctx, instanceName)
	if err != nil {
		return fmt.Errorf("getting node claim by node name: %w", err)
	}
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return nil
	}

	c.markOfferingUnavailable(ctx, nodeClaim, instanceName)

	if err := c.deleteNodeClaim(ctx, nodeClaim); err != nil {
		return fmt.Errorf("deleting node claim: %w", err)
	}

	return nil
}

// markOfferingUnavailable records a spot preemption against the unavailable offerings cache.
//
// The shutdown condition this controller watches fires for any graceful node shutdown, not only
// preemption: on-demand NodeClaims are skipped outright, since an operator-initiated or
// maintenance shutdown of an on-demand node carries no signal about spot capacity in that zone.
// A spot NodeClaim's shutdown isn't necessarily a preemption either (an operator can stop a spot
// VM directly, and GCE terminates rather than live-migrates a spot VM for host maintenance), so
// it's only marked once isPreempted confirms GCE actually reclaimed the instance - otherwise the
// scheduler would be steered away from a zone that is in fact healthy.
func (c *Controller) markOfferingUnavailable(ctx context.Context, nodeClaim *karpv1.NodeClaim, instanceName string) {
	if nodeClaim.Labels[karpv1.CapacityTypeLabelKey] != karpv1.CapacityTypeSpot {
		return
	}

	zone := nodeClaim.Labels[corev1.LabelTopologyZone]
	instanceType := nodeClaim.Labels[corev1.LabelInstanceTypeStable]
	if zone == "" || instanceType == "" {
		log.FromContext(ctx).V(1).Info("skipping unavailable offering marking, nodeclaim is missing zone or instance type label",
			"nodeClaim", nodeClaim.Name, "zone", zone, "instanceType", instanceType)
		return
	}

	if !c.isPreempted(ctx, zone, instanceName) {
		return
	}

	c.unavailableOfferingsCache.MarkUnavailable(ctx, OperationTypePreempted, instanceType, zone, karpv1.CapacityTypeSpot)
}

// isPreempted reports whether GCE recorded a compute.instances.preempted operation against the
// named instance in zone - the documented way to confirm after the fact that a spot VM's
// shutdown was in fact a preemption, since by the time this controller observes the node's
// shutdown condition the instance itself is already gone.
// See: https://cloud.google.com/compute/docs/instances/create-use-spot#detect-preemption
//
// On a lookup error, this conservatively reports false (not confirmed preempted): failing to
// mark a genuine preemption only costs one slightly-suboptimal retry, whereas a false positive
// actively steers the scheduler away from healthy capacity for the unavailable-offering TTL.
func (c *Controller) isPreempted(ctx context.Context, zone, instanceName string) bool {
	targetLink := fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s/instances/%s", c.projectID, zone, instanceName)
	filter := fmt.Sprintf(`operationType="%s" AND targetLink="%s"`, OperationTypePreempted, targetLink)

	ops, err := c.computeService.ZoneOperations.List(c.projectID, zone).Filter(filter).Context(ctx).Do()
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to check for a preemption operation; not marking offering unavailable",
			"zone", zone, "instanceName", instanceName)
		return false
	}
	return len(ops.Items) > 0
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
