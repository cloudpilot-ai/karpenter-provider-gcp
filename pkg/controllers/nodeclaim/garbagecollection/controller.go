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

package garbagecollection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const (
	gcInterval = 2 * time.Minute
	// gcGracePeriod prevents newly created instances from being GC'd before their
	// NodeClaim has been written to the API server.
	gcGracePeriod = 30 * time.Second
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{kubeClient: kubeClient, cloudProvider: cloudProvider}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	instances, err := c.cloudProvider.List(ctx)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("listing instances: %w", err)
	}

	var nodeClaimList karpv1.NodeClaimList
	if err := c.kubeClient.List(ctx, &nodeClaimList); err != nil {
		return reconciler.Result{}, fmt.Errorf("listing nodeclaims: %w", err)
	}

	knownIDs := sets.New[string]()
	for i := range nodeClaimList.Items {
		if id := nodeClaimList.Items[i].Status.ProviderID; id != "" {
			knownIDs.Insert(id)
		}
	}

	var deleteErrs []error
	for _, inst := range instances {
		if inst.Status.ProviderID == "" {
			continue
		}
		if inst.DeletionTimestamp != nil {
			continue
		}
		if knownIDs.Has(inst.Status.ProviderID) {
			continue
		}
		if time.Since(inst.CreationTimestamp.Time) < gcGracePeriod {
			continue
		}
		// Only GC instances that carry the cluster-location label. Instances without it
		// were created by an older Karpenter version and are tracked by the cache as a
		// backward-compatibility measure, but we cannot confirm they belong exclusively
		// to this cluster, so we leave them alone.
		if _, ok := inst.Labels[utils.LabelClusterLocationKey]; !ok {
			continue
		}
		log.FromContext(ctx).Info("garbage collecting orphaned instance", "providerID", inst.Status.ProviderID)
		if err := c.cloudProvider.Delete(ctx, inst); err != nil && !cloudprovider.IsNodeClaimNotFoundError(err) {
			log.FromContext(ctx).Error(err, "failed to delete orphaned instance", "providerID", inst.Status.ProviderID)
			deleteErrs = append(deleteErrs, err)
		}
	}

	// Return any delete errors so controller-runtime applies exponential backoff.
	// All instances are attempted regardless; the error is only returned after the loop.
	return reconciler.Result{RequeueAfter: gcInterval}, errors.Join(deleteErrs...)
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("instance.garbagecollection").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
