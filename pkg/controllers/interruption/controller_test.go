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

package interruption

import (
	"context"
	"testing"

	opmetrics "github.com/awslabs/operatorpkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpevents "sigs.k8s.io/karpenter/pkg/events"
	karpmetrics "sigs.k8s.io/karpenter/pkg/metrics"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

// fakeKubeClient embeds client.Client and overrides only the methods used by the controller.
type fakeKubeClient struct {
	client.Client
	nodes      []corev1.Node
	nodeClaims []karpv1.NodeClaim
	deleted    []client.Object
}

func (f *fakeKubeClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch v := list.(type) {
	case *corev1.NodeList:
		v.Items = f.nodes
	case *karpv1.NodeClaimList:
		v.Items = f.nodeClaims
	}
	return nil
}

func (f *fakeKubeClient) Delete(_ context.Context, obj client.Object, _ ...client.DeleteOption) error {
	f.deleted = append(f.deleted, obj)
	return nil
}

// fakeRecorder captures published events.
type fakeRecorder struct {
	events []karpevents.Event
}

func (f *fakeRecorder) Publish(evts ...karpevents.Event) {
	f.events = append(f.events, evts...)
}

// disruptedCounterValue reads the current value of the NodeClaimsDisruptedTotal counter for the given labels.
func disruptedCounterValue(t *testing.T, labels prometheus.Labels) float64 {
	t.Helper()
	vec := karpmetrics.NodeClaimsDisruptedTotal.(*opmetrics.PrometheusCounter).CounterVec
	c, err := vec.GetMetricWith(labels)
	require.NoError(t, err)
	m := &dto.Metric{}
	require.NoError(t, c.Write(m))
	return m.Counter.GetValue()
}

func TestDeleteNodeClaim_IncrementsDisruptedMetric(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{}
	kubeClient := &fakeKubeClient{}
	c := &Controller{
		kubeClient: kubeClient,
		recorder:   recorder,
	}

	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-nodeclaim",
			Labels: map[string]string{
				karpv1.NodePoolLabelKey:     "test-pool",
				karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeSpot,
			},
		},
	}

	labels := prometheus.Labels{
		karpmetrics.ReasonLabel:       InterruptionReason,
		karpmetrics.NodePoolLabel:     "test-pool",
		karpmetrics.CapacityTypeLabel: karpv1.CapacityTypeSpot,
	}
	before := disruptedCounterValue(t, labels)

	err := c.deleteNodeClaim(context.Background(), nodeClaim)
	require.NoError(t, err)

	require.Equal(t, before+1, disruptedCounterValue(t, labels))
	require.Len(t, kubeClient.deleted, 1)
	require.Len(t, recorder.events, 1)
	require.Equal(t, "TerminatingOnInterruption", recorder.events[0].Reason)
}

func TestDeleteNodeClaim_SkipsAlreadyDeleting(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{}
	kubeClient := &fakeKubeClient{}
	c := &Controller{
		kubeClient: kubeClient,
		recorder:   recorder,
	}

	now := metav1.Now()
	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "terminating-nodeclaim",
			DeletionTimestamp: &now,
		},
	}

	err := c.deleteNodeClaim(context.Background(), nodeClaim)
	require.NoError(t, err)

	require.Empty(t, kubeClient.deleted)
	require.Empty(t, recorder.events)
}

func TestHandleStoppingSpotInstances_DeletesShuttingDownKarpenterNode(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{}
	kubeClient := &fakeKubeClient{
		nodes: []corev1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "spot-node",
					Labels: map[string]string{utils.LabelNodePoolKey: "my-pool"},
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:    corev1.NodeReady,
							Status:  corev1.ConditionFalse,
							Reason:  NodeConditionReasonKubeletNotReady,
							Message: NodeConditionMessageShuttingDown,
						},
					},
				},
			},
		},
		nodeClaims: []karpv1.NodeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "spot-claim",
					Labels: map[string]string{
						karpv1.NodePoolLabelKey:     "my-pool",
						karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeSpot,
					},
				},
				Status: karpv1.NodeClaimStatus{NodeName: "spot-node"},
			},
		},
	}

	c := &Controller{
		kubeClient: kubeClient,
		recorder:   recorder,
	}

	labels := prometheus.Labels{
		karpmetrics.ReasonLabel:       InterruptionReason,
		karpmetrics.NodePoolLabel:     "my-pool",
		karpmetrics.CapacityTypeLabel: karpv1.CapacityTypeSpot,
	}
	before := disruptedCounterValue(t, labels)

	err := c.handleStoppingSpotInstances(context.Background())
	require.NoError(t, err)

	require.Len(t, kubeClient.deleted, 1)
	require.Equal(t, "spot-claim", kubeClient.deleted[0].GetName())
	require.Equal(t, before+1, disruptedCounterValue(t, labels))
}
