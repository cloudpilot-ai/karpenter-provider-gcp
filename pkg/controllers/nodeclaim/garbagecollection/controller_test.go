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

package garbagecollection

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

// fakeKubeClient overrides only the List method used by the GC controller.
type fakeKubeClient struct {
	client.Client
	nodeClaims []karpv1.NodeClaim
}

func (f *fakeKubeClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	ncl, ok := list.(*karpv1.NodeClaimList)
	if !ok {
		panic("fakeKubeClient.List: unexpected type, only *karpv1.NodeClaimList is supported")
	}
	ncl.Items = f.nodeClaims
	return nil
}

// fakeCloudProvider stubs List and Delete; all other methods panic (unused by GC).
type fakeCloudProvider struct {
	instances  []*karpv1.NodeClaim
	deletedIDs []string
}

func (f *fakeCloudProvider) List(_ context.Context) ([]*karpv1.NodeClaim, error) {
	return f.instances, nil
}
func (f *fakeCloudProvider) Delete(_ context.Context, nc *karpv1.NodeClaim) error {
	f.deletedIDs = append(f.deletedIDs, nc.Status.ProviderID)
	return nil
}
func (f *fakeCloudProvider) Create(_ context.Context, _ *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	panic("not implemented")
}
func (f *fakeCloudProvider) Get(_ context.Context, _ string) (*karpv1.NodeClaim, error) {
	panic("not implemented")
}
func (f *fakeCloudProvider) GetInstanceTypes(_ context.Context, _ *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	panic("not implemented")
}
func (f *fakeCloudProvider) IsDrifted(_ context.Context, _ *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	panic("not implemented")
}
func (f *fakeCloudProvider) GetSupportedNodeClasses() []status.Object { return nil }
func (f *fakeCloudProvider) Name() string                             { return "fake" }
func (f *fakeCloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return nil
}
func (f *fakeCloudProvider) LivenessProbe(_ *http.Request) error { return nil }

// newNC creates a NodeClaim with the cluster-location label stamped, matching what
// production Karpenter does for all instances created after this release.
func newNC(providerID string, age time.Duration) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              providerID,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
			Labels:            map[string]string{utils.LabelClusterLocationKey: "us-central1-f"},
		},
	}
	nc.Status.ProviderID = providerID
	return nc
}

// newNCLegacy creates a NodeClaim without the cluster-location label, representing
// instances created by an older Karpenter version before this label was introduced.
func newNCLegacy(providerID string, age time.Duration) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              providerID,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
		},
	}
	nc.Status.ProviderID = providerID
	return nc
}

func newController(cloudInstances []*karpv1.NodeClaim, k8sClaims []karpv1.NodeClaim) *Controller {
	cp := &fakeCloudProvider{instances: cloudInstances}
	return &Controller{
		kubeClient:    &fakeKubeClient{nodeClaims: k8sClaims},
		cloudProvider: cp,
	}
}

func cloudProviderOf(c *Controller) *fakeCloudProvider {
	return c.cloudProvider.(*fakeCloudProvider)
}

func TestGC_DeletesOrphanedInstance(t *testing.T) {
	t.Parallel()

	orphan := newNC("gce://proj/zone/orphan", 2*time.Minute)
	c := newController([]*karpv1.NodeClaim{orphan}, nil)

	_, err := c.Reconcile(context.Background())

	require.NoError(t, err)
	require.Equal(t, []string{"gce://proj/zone/orphan"}, cloudProviderOf(c).deletedIDs)
}

func TestGC_SkipsKnownInstance(t *testing.T) {
	t.Parallel()

	nc := newNC("gce://proj/zone/known", 2*time.Minute)
	c := newController([]*karpv1.NodeClaim{nc}, []karpv1.NodeClaim{*nc})

	_, err := c.Reconcile(context.Background())

	require.NoError(t, err)
	require.Empty(t, cloudProviderOf(c).deletedIDs)
}

func TestGC_SkipsNewInstance(t *testing.T) {
	t.Parallel()

	fresh := newNC("gce://proj/zone/fresh", 5*time.Second)
	c := newController([]*karpv1.NodeClaim{fresh}, nil)

	_, err := c.Reconcile(context.Background())

	require.NoError(t, err)
	require.Empty(t, cloudProviderOf(c).deletedIDs, "instance younger than grace period must not be GC'd")
}

func TestGC_SkipsAlreadyDeletingInstance(t *testing.T) {
	t.Parallel()

	ts := metav1.Now()
	deleting := newNC("gce://proj/zone/deleting", 2*time.Minute)
	deleting.DeletionTimestamp = &ts
	c := newController([]*karpv1.NodeClaim{deleting}, nil)

	_, err := c.Reconcile(context.Background())

	require.NoError(t, err)
	require.Empty(t, cloudProviderOf(c).deletedIDs)
}

// errorOnFirstDeleteProvider returns an error for one specific instance and succeeds for the rest.
type errorOnFirstDeleteProvider struct {
	fakeCloudProvider
	failID string
}

func (e *errorOnFirstDeleteProvider) Delete(ctx context.Context, nc *karpv1.NodeClaim) error {
	if nc.Status.ProviderID == e.failID {
		return fmt.Errorf("transient error")
	}
	return e.fakeCloudProvider.Delete(ctx, nc)
}

func TestGC_SkipsInstanceWithEmptyProviderID(t *testing.T) {
	t.Parallel()

	// An instance with no ProviderID must never be GC'd: it would pass the
	// knownIDs check (empty string is never inserted) and could be a node still
	// initializing before its ProviderID is written back.
	noID := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "no-provider-id",
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-2 * time.Minute)},
		},
	}
	c := newController([]*karpv1.NodeClaim{noID}, nil)

	_, err := c.Reconcile(context.Background())

	require.NoError(t, err)
	require.Empty(t, cloudProviderOf(c).deletedIDs, "instance with empty ProviderID must not be GC'd")
}

func TestGC_ContinuesAfterDeleteError(t *testing.T) {
	t.Parallel()

	first := newNC("gce://proj/zone/first", 2*time.Minute)
	second := newNC("gce://proj/zone/second", 2*time.Minute)
	cp := &errorOnFirstDeleteProvider{
		fakeCloudProvider: fakeCloudProvider{instances: []*karpv1.NodeClaim{first, second}},
		failID:            first.Status.ProviderID,
	}
	c := &Controller{
		kubeClient:    &fakeKubeClient{},
		cloudProvider: cp,
	}

	_, err := c.Reconcile(context.Background())

	require.Error(t, err, "reconcile must return an error when a delete fails so controller-runtime applies backoff")
	require.Equal(t, []string{second.Status.ProviderID}, cp.deletedIDs,
		"second instance must still be attempted even when the first delete fails")
}

func TestGC_SkipsLegacyInstanceWithoutLocationLabel(t *testing.T) {
	t.Parallel()

	// Instances without goog-k8s-cluster-location were created by an older Karpenter
	// version. The cache includes them for backward compatibility, but GC must not touch
	// them — we cannot confirm they belong exclusively to this cluster.
	legacy := newNCLegacy("gce://proj/zone/legacy", 2*time.Minute)
	c := newController([]*karpv1.NodeClaim{legacy}, nil)

	_, err := c.Reconcile(context.Background())

	require.NoError(t, err)
	require.Empty(t, cloudProviderOf(c).deletedIDs, "legacy instance without cluster-location label must not be GC'd")
}
