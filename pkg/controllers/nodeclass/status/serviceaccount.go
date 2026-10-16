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

package status

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/serviceaccount"
)

type ServiceAccount struct {
	provider serviceaccount.Provider
}

func (s *ServiceAccount) Reconcile(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (reconcile.Result, error) {
	account, err := s.provider.Resolve(ctx, nodeClass.Spec.ServiceAccount)
	switch {
	case err != nil:
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeServiceAccountReady, "ServiceAccountResolutionFailed", err.Error())
	case account == "":
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeServiceAccountReady, "ServiceAccountNotFound", "No service account resolved from spec.serviceAccount, DEFAULT_NODEPOOL_SERVICE_ACCOUNT, or the Compute Engine project default")
	default:
		nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeServiceAccountReady)
		return reconcile.Result{}, nil
	}
	return reconcile.Result{RequeueAfter: time.Minute}, nil
}
