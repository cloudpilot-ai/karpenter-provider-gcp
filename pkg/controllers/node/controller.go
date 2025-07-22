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

package node

import (
	"context"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	podutil "sigs.k8s.io/karpenter/pkg/utils/pod"
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, node *corev1.Node) (reconcile.Result, error) {
	// if the node is not ready more then 30, skip
	readyCond, ok := lo.Find(node.Status.Conditions, func(cond corev1.NodeCondition) bool {
		return cond.Type == corev1.NodeReady
	})
	if !ok || readyCond.Status != corev1.ConditionTrue {
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if time.Since(readyCond.LastTransitionTime.Time) < 30*time.Second {
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// check if the node is managed by karpenter
	if !nodeutils.IsManaged(node, c.cloudProvider) {
		return reconcile.Result{}, nil
	}

	// check if the node is empty
	if !c.isEmpty(node) {
		return reconcile.Result{}, nil
	}

	// TODO: it should follow the emptiness disrupt rule of the target nodepool
	log.FromContext(ctx).Info("deleting empty node", "node", node.Name)
	// delete the node
	if err := c.kubeClient.Delete(ctx, node); err != nil {
		return reconcile.Result{Requeue: true}, err
	}

	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.emptiness").
		For(&corev1.Node{}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

var ignorePodsByLabel = map[string]string{
	"k8s-app": "konnectivity-agent",
}

func (c *Controller) isEmpty(node *corev1.Node) bool {
	var pods corev1.PodList
	if err := c.kubeClient.List(context.Background(), &pods, client.InNamespace(node.Namespace), client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return false
	}

	filteredPods := lo.Filter(pods.Items, func(pod corev1.Pod, _ int) bool {
		reschedualbe := podutil.IsReschedulable(&pod)
		if !reschedualbe {
			return false
		}

		for key, value := range ignorePodsByLabel {
			if ignore, ok := pod.Labels[key]; ok && ignore == value {
				return false
			}
		}

		return true
	})

	return len(filteredPods) == 0
}
