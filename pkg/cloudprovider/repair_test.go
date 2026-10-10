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

package cloudprovider

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/node/legacyrepair"
	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"
)

// Record the Kubernetes deletion request, the legacy controller's public effect.
type repairClient struct {
	client.Client
	deleted *karpv1.NodeClaim
}

func (c *repairClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	if claim, ok := object.(*karpv1.NodeClaim); ok {
		c.deleted = claim.DeepCopy()
	}
	return c.Client.Delete(ctx, object, opts...)
}

func TestLegacyRepairReplacesUnhealthyNode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		condition corev1.NodeConditionType
		status    corev1.ConditionStatus
		wait      time.Duration
	}{
		{corev1.NodeReady, corev1.ConditionFalse, 10 * time.Minute},
		{"FrequentContainerdRestart", corev1.ConditionTrue, 30 * time.Minute},
	} {
		t.Run(string(tc.condition), func(t *testing.T) {
			t.Parallel()
			transition := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "unhealthy-node"},
				Spec:       corev1.NodeSpec{ProviderID: "gce://project/zone/unhealthy-node"},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
					Type: tc.condition, Status: tc.status, Reason: "UnknownGKEReason", LastTransitionTime: metav1.NewTime(transition),
				}}},
			}
			claim := &karpv1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "unhealthy-claim"},
				Status:     karpv1.NodeClaimStatus{ProviderID: node.Spec.ProviderID},
			}
			kubeClient := &repairClient{Client: fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node, claim).
				WithIndex(&karpv1.NodeClaim{}, "status.providerID", func(obj client.Object) []string {
					return []string{obj.(*karpv1.NodeClaim).Status.ProviderID}
				}).WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
				return []string{obj.(*corev1.Pod).Spec.NodeName}
			}).Build()}
			clk := clocktesting.NewFakeClock(transition.Add(tc.wait - time.Second))
			controller := legacyrepair.NewController(kubeClient, &CloudProvider{}, clk, reproEvents{})
			ctx := karpopts.ToContext(context.Background(), &karpopts.Options{FeatureGates: karpopts.FeatureGates{NodeRepair: true}, LegacyNodeRepair: true})
			result, err := controller.Reconcile(ctx, node)
			require.NoError(t, err)
			require.Equal(t, time.Second, result.RequeueAfter)
			require.Nil(t, kubeClient.deleted)
			clk.Step(time.Second)
			_, err = controller.Reconcile(ctx, node)
			require.NoError(t, err)
			require.NotNil(t, kubeClient.deleted)
			require.Equal(t, claim.Name, kubeClient.deleted.Name)
			require.Equal(t, clk.Now().Format(time.RFC3339), kubeClient.deleted.Annotations[karpv1.NodeClaimTerminationTimestampAnnotationKey])
		})
	}
}
