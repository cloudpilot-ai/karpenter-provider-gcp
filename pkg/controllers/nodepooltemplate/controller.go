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

	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
)

const (
	finalizer     = "karpenter.k8s.gcp/nodepooltemplate"
	annotationKey = "karpenter.k8s.gcp/gcenodeclass-hash"
)

// Controller for the resource
type Controller struct {
	kubeClient               client.Client
	nodePoolTemplateProvider nodepooltemplate.Provider
}

// NewController is a constructor
func NewController(kubeClient client.Client, nodePoolTemplateProvider nodepooltemplate.Provider) *Controller {
	return &Controller{
		kubeClient:               kubeClient,
		nodePoolTemplateProvider: nodePoolTemplateProvider,
	}
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	// 1. Read the nodepool
	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, nodePool); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Read the gce node class
	gceNodeClass := &v1alpha1.GCENodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, gceNodeClass); err != nil {
		// If the node class doesn't exist, we don't need to do anything.
		// If the node pool is deleted, the finalizer will be removed.
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// 3. Handle deletion
	if !nodePool.DeletionTimestamp.IsZero() {
		return c.delete(ctx, nodePool, gceNodeClass)
	}

	return c.reconcile(ctx, nodePool, gceNodeClass)
}

func (c *Controller) reconcile(ctx context.Context, nodePool *v1.NodePool, gceNodeClass *v1alpha1.GCENodeClass) (reconcile.Result, error) {
	// 4. Add finalizer
	if !controllerutil.ContainsFinalizer(nodePool, finalizer) {
		controllerutil.AddFinalizer(nodePool, finalizer)
		if err := c.kubeClient.Update(ctx, nodePool); err != nil {
			return reconcile.Result{}, err
		}
	}

	// 5. Reconcile the nodepool template
	if err := c.nodePoolTemplateProvider.Create(ctx, gceNodeClass, nodePool); err != nil {
		return reconcile.Result{}, err
	}

	// 6. Calculate hash and annotate nodepool
	hash, err := c.calculateHash(gceNodeClass)
	if err != nil {
		return reconcile.Result{}, err
	}

	if nodePool.Annotations[annotationKey] != hash {
		patch := client.MergeFrom(nodePool.DeepCopy())
		if nodePool.Annotations == nil {
			nodePool.Annotations = make(map[string]string)
		}
		nodePool.Annotations[annotationKey] = hash
		if err := c.kubeClient.Patch(ctx, nodePool, patch); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

func (c *Controller) delete(ctx context.Context, nodePool *v1.NodePool, gceNodeClass *v1alpha1.GCENodeClass) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(nodePool, finalizer) {
		return reconcile.Result{}, nil
	}

	if err := c.nodePoolTemplateProvider.Delete(ctx, gceNodeClass); err != nil {
		return reconcile.Result{}, err
	}

	controllerutil.RemoveFinalizer(nodePool, finalizer)
	if err := c.kubeClient.Update(ctx, nodePool); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (c *Controller) calculateHash(gceNodeClass *v1alpha1.GCENodeClass) (string, error) {
	hash, err := hashstructure.Hash(gceNodeClass.Spec, hashstructure.FormatV2, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", hash), nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		For(&v1.NodePool{}).
		Watches(
			&v1alpha1.GCENodeClass{},
			handler.EnqueueRequestsFromMapFunc(c.nodeClassToNodePool),
		).
		Named("nodepooltemplate").
		Complete(c)
}

func (c *Controller) nodeClassToNodePool(ctx context.Context, obj client.Object) []reconcile.Request {
	var nodePools v1.NodePoolList
	if err := c.kubeClient.List(ctx, &nodePools); err != nil {
		return nil
	}

	return lo.FilterMap(nodePools.Items, func(np v1.NodePool, _ int) (reconcile.Request, bool) {
		if np.Spec.Template.Spec.NodeClassRef.Name == obj.GetName() {
			return reconcile.Request{NamespacedName: types.NamespacedName{Name: np.Name}}, true
		}
		return reconcile.Request{}, false
	})
}
