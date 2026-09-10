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
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
)

const subnetRangeStatusRequeue = 5 * time.Minute

type SubnetRange struct {
	gkeProvider gke.Provider
}

func (s *SubnetRange) Reconcile(ctx context.Context, nodeClass *v1alpha1.GCENodeClass) (reconcile.Result, error) {
	cluster, err := s.gkeProvider.GetClusterConfig(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "getting cluster config for subnet ranges")
		return reconcile.Result{}, err
	}

	names := nodeClass.PodSubnetRangeNames()
	if len(names) == 0 {
		names = []string{gke.DefaultPodRangeName(cluster)}
	}
	status := make([]v1alpha1.SubnetRangeStatus, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		entry := v1alpha1.SubnetRangeStatus{Name: name}
		if util, ok := gke.PodRangeUtilization(cluster, name); ok {
			u := strconv.FormatFloat(util, 'f', -1, 64)
			entry.Utilization = &u
		}
		status = append(status, entry)
	}
	nodeClass.Status.SubnetRanges = status
	return reconcile.Result{RequeueAfter: subnetRangeStatusRequeue}, nil
}
