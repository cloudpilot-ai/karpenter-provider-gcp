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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	sandboxcontract "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/sandbox"
)

func sandboxPool() *karpv1.NodePool {
	return &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{ObjectMeta: karpv1.ObjectMeta{Labels: map[string]string{"team": "compute"}}, Spec: karpv1.NodeClaimTemplateSpec{NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "sandbox"}, Taints: []corev1.Taint{{Key: "dedicated", Value: "test", Effect: corev1.TaintEffectNoSchedule}}}}}}
}

func sandboxClass() *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "sandbox"}, Spec: v1alpha1.GCENodeClassSpec{SandboxConfig: &v1alpha1.SandboxConfig{Type: "GVISOR"}}}
}

// poolClient stores only the domain objects used by this reconciler. The Patch
// boundary records the real optimistic patch; it does not emulate an API server.
type poolClient struct {
	client.Client
	pools      map[string]*karpv1.NodePool
	classes    map[string]*v1alpha1.GCENodeClass
	patchCount int
	patchData  []byte
	patchErr   error
}

func testClient(t *testing.T, objects ...client.Object) *poolClient {
	t.Helper()
	kc := &poolClient{pools: map[string]*karpv1.NodePool{}, classes: map[string]*v1alpha1.GCENodeClass{}}
	for _, obj := range objects {
		switch obj := obj.(type) {
		case *karpv1.NodePool:
			pool := obj.DeepCopy()
			pool.ResourceVersion = "1"
			kc.pools[pool.Name] = pool
		case *v1alpha1.GCENodeClass:
			kc.classes[obj.Name] = obj.DeepCopy()
		}
	}
	return kc
}
func (c *poolClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	switch obj := obj.(type) {
	case *karpv1.NodePool:
		if pool := c.pools[key.Name]; pool != nil {
			*obj = *pool.DeepCopy()
			return nil
		}
	case *v1alpha1.GCENodeClass:
		if class := c.classes[key.Name]; class != nil {
			*obj = *class.DeepCopy()
			return nil
		}
	}
	return apierrors.NewNotFound(schema.GroupResource{Resource: "test-object"}, key.Name)
}
func (c *poolClient) List(_ context.Context, obj client.ObjectList, _ ...client.ListOption) error {
	list := obj.(*karpv1.NodePoolList)
	for _, pool := range c.pools {
		list.Items = append(list.Items, *pool.DeepCopy())
	}
	return nil
}
func (c *poolClient) Patch(_ context.Context, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
	c.patchCount++
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	c.patchData = data
	if c.patchErr != nil {
		return c.patchErr
	}
	c.pools[obj.GetName()] = obj.(*karpv1.NodePool).DeepCopy()
	return nil
}

func TestControllerPersistsIsolationIdempotentlyAndUsesOptimisticLock(t *testing.T) {
	ctx := context.Background()
	pool := sandboxPool()
	kc := testClient(t, pool, sandboxClass())
	controller := NewController(kc)
	require.NoError(t, kc.Get(ctx, client.ObjectKeyFromObject(pool), pool))
	_, err := controller.Reconcile(ctx, pool)
	require.NoError(t, err)
	stored := &karpv1.NodePool{}
	require.NoError(t, kc.Get(ctx, client.ObjectKeyFromObject(pool), stored))
	require.NoError(t, sandboxcontract.ValidatePool(stored, true))
	require.Equal(t, "compute", stored.Spec.Template.Labels["team"])
	require.Equal(t, pool.Spec.Template.Spec.Taints[0], stored.Spec.Template.Spec.Taints[0])
	require.JSONEq(t, `{"label":true,"taint":true}`, stored.Annotations[OwnershipAnnotation])
	require.NotContains(t, pool.Spec.Template.Labels, v1alpha1.LabelSandboxRuntime, "reconcile must not mutate its input")
	_, err = controller.Reconcile(ctx, stored)
	require.NoError(t, err)
	again := &karpv1.NodePool{}
	require.NoError(t, kc.Get(ctx, client.ObjectKeyFromObject(pool), again))
	require.Equal(t, 1, kc.patchCount, "idempotent reconciliation must not write")

	// Lists are replaced by merge patches, so the resourceVersion precondition
	// is essential. A conflict must propagate, never fall back to an unsafe patch.
	var patch struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(kc.patchData, &patch))
	require.Equal(t, "1", patch.Metadata.ResourceVersion)
	stale := stored.DeepCopy()
	stale.Spec.Template.Spec.NodeClassRef = nil
	kc.patchErr = apierrors.NewConflict(schema.GroupResource{Resource: "nodepools"}, "pool", errors.New("changed"))
	_, err = controller.Reconcile(ctx, stale)
	require.True(t, apierrors.IsConflict(err), "%v", err)
	require.NoError(t, kc.Get(ctx, client.ObjectKeyFromObject(pool), again))
	require.Equal(t, stored, again, "conflict must leave persisted fields untouched")
}

func TestControllerTracksLabelAndTaintOwnershipSeparately(t *testing.T) {
	for _, tc := range []struct {
		name         string
		label, taint bool
	}{
		{name: "neither preexisting"}, {name: "user label", label: true}, {name: "user taint", taint: true}, {name: "both user owned", label: true, taint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := sandboxPool()
			if tc.label {
				pool.Spec.Template.Labels[v1alpha1.LabelSandboxRuntime] = "gvisor"
			}
			if tc.taint {
				pool.Spec.Template.Spec.Taints = append(pool.Spec.Template.Spec.Taints, sandboxcontract.RuntimeTaint())
			}
			require.NoError(t, reconcileFields(pool, true))
			require.NoError(t, sandboxcontract.ValidatePool(pool, true))
			require.NoError(t, reconcileFields(pool, false))
			_, hasLabel := pool.Spec.Template.Labels[v1alpha1.LabelSandboxRuntime]
			require.Equal(t, tc.label, hasLabel)
			require.Equal(t, 1+boolInt(tc.taint), len(pool.Spec.Template.Spec.Taints))
			require.NotContains(t, pool.Annotations, OwnershipAnnotation)
			require.Equal(t, "compute", pool.Spec.Template.Labels["team"])
		})
	}
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestControllerPreservesConflictingUserIntent(t *testing.T) {
	pool := sandboxPool()
	pool.Spec.Template.Labels[v1alpha1.LabelSandboxRuntime] = "other"
	pool.Spec.Template.Spec.Taints = append(pool.Spec.Template.Spec.Taints, corev1.Taint{Key: v1alpha1.LabelSandboxRuntime, Value: "other", Effect: corev1.TaintEffectNoExecute})
	before := pool.DeepCopy()
	require.NoError(t, reconcileFields(pool, true))
	require.Equal(t, before, pool)
	require.Error(t, sandboxcontract.ValidatePool(pool, true))

	pool = sandboxPool()
	require.NoError(t, reconcileFields(pool, true))
	pool.Spec.Template.Labels[v1alpha1.LabelSandboxRuntime] = "user-edit"
	pool.Spec.Template.Spec.Taints[1].Value = "user-edit"
	require.NoError(t, reconcileFields(pool, false))
	require.Equal(t, "user-edit", pool.Spec.Template.Labels[v1alpha1.LabelSandboxRuntime])
	require.Equal(t, "user-edit", pool.Spec.Template.Spec.Taints[1].Value)
	require.NotContains(t, pool.Annotations, OwnershipAnnotation)
}

func TestControllerCleansOwnedFieldsAfterReferenceChanges(t *testing.T) {
	for _, reason := range []string{"disabled", "ordinary ref", "missing ref", "deleting ref", "unmanaged ref"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			pool := sandboxPool()
			require.NoError(t, reconcileFields(pool, true))
			class := sandboxClass()
			switch reason {
			case "disabled":
				class.Spec.SandboxConfig = nil
			case "ordinary ref":
				pool.Spec.Template.Spec.NodeClassRef.Name = "ordinary"
			case "missing ref":
				pool.Spec.Template.Spec.NodeClassRef.Name = "missing"
			case "deleting ref":
				class.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
				class.Finalizers = []string{"retain"}
			case "unmanaged ref":
				pool.Spec.Template.Spec.NodeClassRef.Kind = "Other"
			}
			ordinary := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "ordinary"}}
			kc := testClient(t, pool, class, ordinary)
			require.NoError(t, kc.Get(ctx, client.ObjectKeyFromObject(pool), pool))
			_, err := NewController(kc).Reconcile(ctx, pool)
			require.NoError(t, err)
			stored := &karpv1.NodePool{}
			require.NoError(t, kc.Get(ctx, client.ObjectKeyFromObject(pool), stored))
			require.NoError(t, sandboxcontract.ValidatePool(stored, false))
			require.NotContains(t, stored.Annotations, OwnershipAnnotation)
			require.Equal(t, "compute", stored.Spec.Template.Labels["team"])
		})
	}
}

func TestControllerClassWatchEnqueuesOnlyReferencingManagedPools(t *testing.T) {
	pool := sandboxPool()
	unrelated := sandboxPool()
	unrelated.Name = "unrelated"
	unrelated.Spec.Template.Spec.NodeClassRef.Name = "other"
	unmanaged := sandboxPool()
	unmanaged.Name = "unmanaged"
	unmanaged.Spec.Template.Spec.NodeClassRef.Group = "other"
	requests := NewController(testClient(t, pool, unrelated, unmanaged)).enqueueNodePools(context.Background(), sandboxClass())
	require.Len(t, requests, 1)
	require.Equal(t, "pool", requests[0].Name)
}
