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

package nodepooltemplate

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
)

// fallbackThreshold is the number of consecutive Sync failures before the fallback pool
// is created. With the custom rate limiter (base=30s), this gives roughly 7.5 minutes of
// retries before falling back: 30s + 60s + 120s + 240s = 450s elapsed on the 5th failure.
const fallbackThreshold = 5

type Controller struct {
	nodePoolTemplateProvider nodepooltemplate.Provider
	consecutiveFailures      int
}

func NewController(nodePooltemplateProvider nodepooltemplate.Provider) *Controller {
	return &Controller{
		nodePoolTemplateProvider: nodePooltemplateProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodepooltemplate")
	if err := c.nodePoolTemplateProvider.Sync(ctx); err != nil {
		c.consecutiveFailures++
		if c.consecutiveFailures >= fallbackThreshold {
			log.FromContext(ctx).Info("retry limit reached, creating fallback pool",
				"attempts", c.consecutiveFailures,
				"fallback", nodepooltemplate.KarpenterFallbackNodePoolTemplate)
			if createErr := c.nodePoolTemplateProvider.EnsureFallbackPool(ctx); createErr != nil {
				return reconciler.Result{}, fmt.Errorf("creating fallback pool: %w", createErr)
			}
		}
		return reconciler.Result{}, err
	}
	c.consecutiveFailures = 0
	return reconciler.Result{RequeueAfter: 12 * time.Minute}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodepooltemplate").
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				30*time.Second, 10*time.Minute),
		}).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
