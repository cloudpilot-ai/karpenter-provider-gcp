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
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
)

type Controller struct {
	nodePoolTemplateProvider nodepooltemplate.Provider
}

func NewController(nodePooltemplateProvider nodepooltemplate.Provider) *Controller {
	return &Controller{
		nodePoolTemplateProvider: nodePooltemplateProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodepooltemplate")
	if err := c.nodePoolTemplateProvider.Sync(ctx); err != nil {
		if createErr := c.nodePoolTemplateProvider.EnsureFallbackPool(ctx); createErr != nil {
			return reconciler.Result{}, fmt.Errorf("creating fallback pool: %w", createErr)
		}
		// Fallback pool was created or already exists; poll until it becomes RUNNING.
		// Returning RequeueAfter (not an error) bypasses the exponential backoff so
		// we check every 15s instead of backing off to minutes.
		return reconciler.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return reconciler.Result{RequeueAfter: 12 * time.Minute}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodepooltemplate").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
