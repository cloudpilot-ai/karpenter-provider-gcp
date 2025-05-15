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
	"github.com/go-openapi/swag"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
	operationClient           *computev1.RegionOperationsClient
	credential                auth.Credential
}

func NewController(kubeClient client.Client, recorder events.Recorder,
	unavailableOfferingsCache *cache.UnavailableOfferings,
	metadataClient *metadata.Client,
	operationClient *computev1.RegionOperationsClient,
	credential auth.Credential) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		recorder:   recorder,

		unavailableOfferingsCache: unavailableOfferingsCache,
		metadataClient:            metadataClient,
		operationClient:           operationClient,
		credential:                credential,
	}
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	// Refer to https://cloud.google.com/compute/docs/instances/create-use-spot#detect-preemption
	// We need to check if the instance is preempted and delete the nodeClaim from the operation event,
	// In this stage, we only detect preempted operation here.
	it := c.operationClient.List(ctx, &computepb.ListRegionOperationsRequest{
		Project: c.credential.ProjectID,
		Region:  c.credential.Region,
		Filter:  swag.String(OperationTypePreempted),
	})

	instanceNames := []string{}
	for {
		op, err := it.Next()
		if err != nil {
			log.FromContext(ctx).Error(err, "listing operations")
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
		WithEventFilter(predicate.NewTypedPredicateFuncs(func(obj client.Object) bool {
			if label, ok := obj.GetLabels()[karpv1.CapacityTypeLabelKey]; !ok || label != karpv1.CapacityTypeSpot {
				return false
			}

			return true
		})).
		Complete(c)
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
