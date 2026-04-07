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
)

const (
	gcInterval    = 2 * time.Minute
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

	knownIDs := sets.NewString()
	for i := range nodeClaimList.Items {
		if id := nodeClaimList.Items[i].Status.ProviderID; id != "" {
			knownIDs.Insert(id)
		}
	}

	for _, nc := range instances {
		if nc.DeletionTimestamp != nil {
			continue
		}
		if knownIDs.Has(nc.Status.ProviderID) {
			continue
		}
		if time.Since(nc.CreationTimestamp.Time) < gcGracePeriod {
			continue
		}
		log.FromContext(ctx).Info("garbage collecting orphaned instance", "providerID", nc.Status.ProviderID)
		if err := c.cloudProvider.Delete(ctx, nc); err != nil && !cloudprovider.IsNodeClaimNotFoundError(err) {
			log.FromContext(ctx).Error(err, "failed to delete orphaned instance", "providerID", nc.Status.ProviderID)
		}
	}

	return reconciler.Result{RequeueAfter: gcInterval}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclaim.garbagecollection").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
