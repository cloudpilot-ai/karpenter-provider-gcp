/*
Copyright 2026 The CloudPilot AI Authors.

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

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/awslabs/operatorpkg/reasonable"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	sandboxcontract "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/sandbox"
)

var OwnershipAnnotation = apis.Group + "/sandbox-ownership"

type ownership struct {
	Label bool `json:"label,omitempty"`
	Taint bool `json:"taint,omitempty"`
}

type Controller struct{ kubeClient client.Client }

func NewController(kubeClient client.Client) *Controller { return &Controller{kubeClient: kubeClient} }

func managed(pool *karpv1.NodePool) bool {
	ref := pool.Spec.Template.Spec.NodeClassRef
	return ref != nil && ref.Group == apis.Group && ref.Kind == "GCENodeClass"
}

func (c *Controller) Reconcile(ctx context.Context, pool *karpv1.NodePool) (reconcile.Result, error) {
	if !pool.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	if !managed(pool) && pool.Annotations[OwnershipAnnotation] == "" {
		return reconcile.Result{}, nil
	}
	enabled := false
	if managed(pool) {
		class := &v1alpha1.GCENodeClass{}
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: pool.Spec.Template.Spec.NodeClassRef.Name}, class); err != nil {
			if !errors.IsNotFound(err) {
				return reconcile.Result{}, err
			}
		} else {
			enabled = class.DeletionTimestamp.IsZero() && class.SandboxEnabled()
		}
	}
	desired := pool.DeepCopy()
	if err := reconcileFields(desired, enabled); err != nil {
		return reconcile.Result{}, err
	}
	if equality.Semantic.DeepEqual(pool, desired) {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, client.IgnoreNotFound(c.kubeClient.Patch(ctx, desired, client.MergeFromWithOptions(pool, client.MergeFromWithOptimisticLock{})))
}

func reconcileFields(pool *karpv1.NodePool, enabled bool) error {
	owned := ownership{}
	if raw := pool.Annotations[OwnershipAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &owned); err != nil {
			return fmt.Errorf("invalid sandbox ownership annotation")
		}
	}
	owned.Label = reconcileLabel(pool, enabled, owned.Label)
	owned.Taint = reconcileTaint(pool, enabled, owned.Taint)
	if !owned.Label && !owned.Taint {
		delete(pool.Annotations, OwnershipAnnotation)
		return nil
	}
	raw, err := json.Marshal(owned)
	if err != nil {
		return err
	}
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[OwnershipAnnotation] = string(raw)
	return nil
}

func (c *Controller) enqueueNodePools(ctx context.Context, obj client.Object) []reconcile.Request {
	pools := &karpv1.NodePoolList{}
	if err := c.kubeClient.List(ctx, pools); err != nil {
		log.FromContext(ctx).Error(err, "listing NodePools for sandbox reconciliation")
		return nil
	}
	var requests []reconcile.Request
	for i := range pools.Items {
		pool := &pools.Items[i]
		if managed(pool) && pool.Spec.Template.Spec.NodeClassRef.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: pool.Name}})
		}
	}
	return requests
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodepool.sandbox").
		For(&karpv1.NodePool{}).
		Watches(&v1alpha1.GCENodeClass{}, handler.EnqueueRequestsFromMapFunc(c.enqueueNodePools)).
		WithOptions(controller.Options{RateLimiter: reasonable.RateLimiter(), MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

func hasRuntimeKey(taint corev1.Taint) bool { return taint.Key == v1alpha1.LabelSandboxRuntime }
func isRuntimeTaint(taint corev1.Taint) bool {
	return taint.Key == v1alpha1.LabelSandboxRuntime && taint.Value == v1alpha1.SandboxRuntimeGVISOR && taint.Effect == corev1.TaintEffectNoSchedule
}

func reconcileLabel(pool *karpv1.NodePool, enabled, owned bool) bool {
	labels := pool.Spec.Template.Labels
	value, present := labels[v1alpha1.LabelSandboxRuntime]
	owned = owned && present && value == v1alpha1.SandboxRuntimeGVISOR
	if !enabled {
		if owned {
			delete(labels, v1alpha1.LabelSandboxRuntime)
		}
		return false
	}
	if present {
		return owned
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[v1alpha1.LabelSandboxRuntime] = v1alpha1.SandboxRuntimeGVISOR
	pool.Spec.Template.Labels = labels
	return true
}

func reconcileTaint(pool *karpv1.NodePool, enabled, owned bool) bool {
	taints := pool.Spec.Template.Spec.Taints
	owned = owned && slices.ContainsFunc(taints, isRuntimeTaint)
	if !enabled {
		if owned {
			pool.Spec.Template.Spec.Taints = slices.DeleteFunc(taints, isRuntimeTaint)
		}
		return false
	}
	if slices.ContainsFunc(taints, hasRuntimeKey) {
		return owned
	}
	pool.Spec.Template.Spec.Taints = append(pool.Spec.Template.Spec.Taints, sandboxcontract.RuntimeTaint())
	return true
}
