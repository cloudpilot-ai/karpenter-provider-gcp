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

package status_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	operatorstatus "github.com/awslabs/operatorpkg/status"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	nodeclassstatus "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/nodeclass/status"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/serviceaccount"
)

func TestNodeClassServiceAccountRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var phase atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch phase.Load() {
		case 0:
			_, _ = w.Write([]byte(`{}`))
		case 1:
			http.Error(w, "project unavailable", http.StatusForbidden)
		default:
			_, _ = w.Write([]byte(`{"defaultServiceAccount":"default@example.com"}`))
		}
	}))
	t.Cleanup(server.Close)
	computeClient, err := compute.NewService(ctx, option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	clock := &testClock{now: time.Now()}
	provider, err := serviceaccount.NewProvider(ctx, computeClient, "project", "", clock)
	require.NoError(t, err)
	kubeClient := &statusClient{}
	controller := nodeclassstatus.NewController(kubeClient, images{}, cluster{}, nil, provider)
	nodeClass := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default", Finalizers: []string{v1alpha1.TerminationFinalizer}}}

	result, err := controller.Reconcile(ctx, nodeClass)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.True(t, kubeClient.stored.StatusConditions().Get(v1alpha1.ConditionTypeServiceAccountReady).IsFalse())
	require.True(t, kubeClient.stored.StatusConditions().Get(operatorstatus.ConditionReady).IsFalse())
	require.True(t, kubeClient.stored.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).IsTrue())

	phase.Store(1)
	clock.now = clock.now.Add(result.RequeueAfter)
	result, err = controller.Reconcile(ctx, nodeClass)
	require.NoError(t, err)
	require.Positive(t, result.RequeueAfter)
	require.True(t, kubeClient.stored.StatusConditions().Get(operatorstatus.ConditionReady).IsFalse())
	require.Contains(t, kubeClient.stored.StatusConditions().Get(v1alpha1.ConditionTypeServiceAccountReady).Message, "getting Compute Engine default service account")

	// Explicit NodeClass accounts remain usable while project discovery is failing.
	explicit := nodeClass.DeepCopy()
	explicit.Spec.ServiceAccount = "explicit@example.com"
	_, err = controller.Reconcile(ctx, explicit)
	require.NoError(t, err)
	require.True(t, kubeClient.stored.StatusConditions().Get(operatorstatus.ConditionReady).IsTrue())

	phase.Store(2)
	clock.now = clock.now.Add(result.RequeueAfter)
	_, err = controller.Reconcile(ctx, nodeClass)
	require.NoError(t, err)
	require.True(t, kubeClient.stored.StatusConditions().Get(v1alpha1.ConditionTypeServiceAccountReady).IsTrue())
	require.True(t, kubeClient.stored.StatusConditions().Get(operatorstatus.ConditionReady).IsTrue())
}

type testClock struct {
	clock.RealClock
	now time.Time
}

func (c *testClock) Now() time.Time { return c.now }

// Record writes at the Kubernetes API boundary, rather than only checking the input object.
type statusClient struct {
	client.Client
	stored *v1alpha1.GCENodeClass
}

type statusWriter struct {
	client.SubResourceWriter
	owner *statusClient
}

func (c *statusClient) Status() client.SubResourceWriter { return &statusWriter{owner: c} }
func (w *statusWriter) Patch(_ context.Context, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
	w.owner.stored = obj.(*v1alpha1.GCENodeClass).DeepCopy()
	return nil
}

type images struct{}

func (images) List(context.Context, *v1alpha1.GCENodeClass) (imagefamily.Images, error) {
	return imagefamily.Images{{SourceImage: "projects/test/global/images/cos"}}, nil
}

type cluster struct{ gke.Provider }

func (cluster) GetClusterConfig(context.Context) (*container.Cluster, error) {
	return &container.Cluster{}, nil
}
