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
	"net/url"
	"strings"
	"time"

	computev1 "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/compute/metadata"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/go-openapi/swag"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	interruptionevents "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/interruption/events"
)

const (
	OperationTypePreempted = "compute.instances.preempted"
)

// Controller is an GCP interruption controller.
type Controller struct {
	kubeClient client.Client
	recorder   events.Recorder

	unavailableOfferingsCache *cache.UnavailableOfferings
	metadataClient            *metadata.Client
	zoneOperationClient       *computev1.ZoneOperationsClient
	credential                auth.Credential
}

func NewController(kubeClient client.Client, recorder events.Recorder,
	unavailableOfferingsCache *cache.UnavailableOfferings,
	metadataClient *metadata.Client,
	zoneOperationClient *computev1.ZoneOperationsClient,
	credential auth.Credential) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		recorder:   recorder,

		unavailableOfferingsCache: unavailableOfferingsCache,
		metadataClient:            metadataClient,
		zoneOperationClient:       zoneOperationClient,
		credential:                credential,
	}
}

// getZonesFromNodes lists all zones by inspecting node labels
func getZonesFromNodes(ctx context.Context, kubeClient client.Client) ([]string, error) {
	nodeList := &corev1.NodeList{}
	if err := kubeClient.List(ctx, nodeList, &client.ListOptions{}); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	zoneSet := make(map[string]struct{})
	for _, node := range nodeList.Items {
		zone := node.Labels["topology.kubernetes.io/zone"]
		if zone == "" {
			// fallback to legacy label
			zone = node.Labels["failure-domain.beta.kubernetes.io/zone"]
		}
		if zone != "" {
			zoneSet[zone] = struct{}{}
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

	for _, zone := range zones {
		// Refer to https://cloud.google.com/compute/docs/instances/create-use-spot#detect-preemption
		// We need to check if the instance is preempted and delete the nodeClaim from the operation event,
		// In this stage, we only detect preempted operation here.
		it := c.zoneOperationClient.List(ctx, &computepb.ListZoneOperationsRequest{
			Project: c.credential.ProjectID,
			Zone:    zone,
			Filter:  swag.String(fmt.Sprintf(`operationType="%s"`, OperationTypePreempted)),
		})

		instanceNames := []string{}
		for {
			op, err := it.Next()
			if err != nil {
				log.FromContext(ctx).Info("listing operations warning", err)
				break
			}
			targetLink := op.GetTargetLink()
			instanceName, err := extractInstanceName(targetLink)
			if err != nil {
				log.FromContext(ctx).Error(err, "extracting instance name")
				break
			}
			instanceNames = append(instanceNames, instanceName)
		}

		errs := make([]error, len(instanceNames))
		workqueue.ParallelizeUntil(ctx, 10, len(instanceNames), func(i int) {
			instanceName := instanceNames[i]
			nodeClaim, err := c.getNodeClaimByNodeName(ctx, instanceName)
			if err != nil {
				errs[i] = fmt.Errorf("getting node claim by node name: %w", err)
				return
			}
			zone := nodeClaim.Labels[corev1.LabelTopologyZone]
			instanceType := nodeClaim.Labels[corev1.LabelInstanceTypeStable]
			if zone != "" && instanceType != "" {
				c.unavailableOfferingsCache.MarkUnavailable(ctx, OperationTypePreempted, instanceType, zone, karpv1.CapacityTypeSpot)
			}

			if err := c.deleteNodeClaim(ctx, nodeClaim); err != nil {
				errs[i] = fmt.Errorf("deleting node claim: %w", err)
			}
		})
		if err := multierr.Combine(errs...); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Will requeue after 3 seconds and try again
	return reconcile.Result{RequeueAfter: 3 * time.Second}, nil
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
