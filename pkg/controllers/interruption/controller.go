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
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	computev1 "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/compute/metadata"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/go-openapi/swag"
	"go.uber.org/multierr"
	"google.golang.org/api/iterator"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	interruptionevents "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/interruption/events"
	instance "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const (
	OperationTypePreempted = "compute.instances.preempted"
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
	instanceProvider instance.Provider) *Controller {
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

// getZonesFromNodes lists all zones by inspecting node labels
func getZonesFromNodes(ctx context.Context, kubeClient client.Client) ([]string, error) {
	nodeList := &corev1.NodeList{}
	if err := kubeClient.List(ctx, nodeList, &client.ListOptions{}); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	zoneSet := sets.Set[string]{}
	for _, node := range nodeList.Items {
		zone := node.Labels[corev1.LabelTopologyZone]
		if zone == "" {
			// fallback to legacy label
			zone = node.Labels[corev1.LabelFailureDomainBetaZone]
		}
		if zone != "" {
			zoneSet.Insert(zone)
		}
	}

	// convert set to sorted slice
	var zones []string
	for zone := range zoneSet {
		zones = append(zones, zone)
	}

	return zones, nil
}

func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	// Get all zones from nodes
	zones, err := getZonesFromNodes(ctx, c.kubeClient)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting zones: %w", err)
	}

	if err := c.handleSpotInterruptionEvents(ctx, zones); err != nil {
		return reconcile.Result{}, fmt.Errorf("handling spot interruption events: %w", err)
	}

	if err := c.handleStoppedSpotInstances(ctx); err != nil {
		return reconcile.Result{}, fmt.Errorf("handling stopped spot instances: %w", err)
	}

	// Will requeue after 3 seconds and try again
	return reconcile.Result{RequeueAfter: 3 * time.Second}, nil
}

func (c *Controller) handleStoppedSpotInstances(ctx context.Context) error {
	nodes := &corev1.NodeList{}
	if err := c.kubeClient.List(ctx, nodes, &client.ListOptions{}); err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	for i := range nodes.Items {
		node := nodes.Items[i]
		if node.Labels == nil || node.Labels[utils.LabelNodePoolKey] == "" {
			continue
		}

		providerID := node.Spec.ProviderID
		if providerID == "" {
			continue
		}

		ins, err := c.instanceProvider.Get(ctx, providerID)
		if err != nil {
			if cloudprovider.IsNodeClaimNotFoundError(err) {
				continue
			}

			return fmt.Errorf("getting instance: %w", err)
		}

		if ins.Status == instance.InstanceStatusStopped || ins.Status == instance.InstanceStatusTerminated {
			if err := c.cleanNodeClaimByInstanceName(ctx, ins.Name, false); err != nil {
				return fmt.Errorf("cleaning node claim: %w", err)
			}
		}
	}

	return nil
}

func (c *Controller) handleSpotInterruptionEvents(ctx context.Context, zones []string) error {
	instanceNames := []string{}
	instanceNamesLock := sync.Mutex{}
	handler := func(zone string) {
		// Refer to https://cloud.google.com/compute/docs/instances/create-use-spot#detect-preemption
		// We need to check if the instance is preempted and delete the nodeClaim from the operation event,
		// In this stage, we only detect preempted operation here.
		it := c.zoneOperationClient.List(ctx, &computepb.ListZoneOperationsRequest{
			Project: c.credential.ProjectID,
			Zone:    zone,
			Filter:  swag.String(fmt.Sprintf(`operationType="%s"`, OperationTypePreempted)),
		})

		for {
			op, err := it.Next()
			if err != nil {
				if !errors.Is(err, iterator.Done) {
					log.FromContext(ctx).Info("listing operations warning", "zone", zone, "error", err)
				}
				break
			}
			targetLink := op.GetTargetLink()
			instanceName, err := extractInstanceName(targetLink)
			if err != nil {
				log.FromContext(ctx).Error(err, "extracting instance name")
				break
			}
			// ignore the instance if the name is not found in the clter nodes
			if !c.isInstanceFromKarpenter(ctx, instanceName) {
				continue
			}

			instanceNamesLock.Lock()
			instanceNames = append(instanceNames, instanceName)
			instanceNamesLock.Unlock()
		}
	}

	workqueue.ParallelizeUntil(ctx, 10, len(zones), func(zoneIndex int) {
		handler(zones[zoneIndex])
	})

	errs := make([]error, len(instanceNames))
	workqueue.ParallelizeUntil(ctx, 10, len(instanceNames), func(i int) {
		instanceName := instanceNames[i]
		if err := c.cleanNodeClaimByInstanceName(ctx, instanceName, true); err != nil {
			errs[i] = fmt.Errorf("deleting node claim: %w", err)
		}
	})
	return multierr.Combine(errs...)
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

func (c *Controller) isInstanceFromKarpenter(ctx context.Context, instanceName string) bool {
	var node corev1.Node
	err := c.kubeClient.Get(ctx, client.ObjectKey{Name: instanceName}, &node)
	if err == nil {
		// The node is in the cluster, but does it have the karpenter label?
		if node.Labels == nil || node.Labels[utils.LabelNodePoolKey] == "" {
			return false
		}
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	log.FromContext(ctx).Error(err, "getting node", "node", instanceName)
	return false
}

func extractInstanceName(targetLink string) (string, error) {
	parsedURL, err := url.Parse(targetLink)
	if err != nil {
		return "", err
	}
	segments := strings.Split(parsedURL.Path, "/")
	if len(segments) < 1 {
		return "", fmt.Errorf("invalid targetLink: %s", targetLink)
	}
	return segments[len(segments)-1], nil
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
	log.FromContext(ctx).Info("initiating delete from interruption message")
	c.recorder.Publish(interruptionevents.TerminatingOnInterruption(nodeClaim)...)
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
