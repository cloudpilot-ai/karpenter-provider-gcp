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
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloud "sigs.k8s.io/karpenter/pkg/cloudprovider"
	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/nodeclaim/garbagecollection"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

type lifecycleInstances struct {
	instance.Provider
	instances []*instance.Instance
	err       error
}

func (p *lifecycleInstances) Get(_ context.Context, providerID string) (*instance.Instance, error) {
	if p.err != nil {
		return nil, p.err
	}
	for _, inst := range p.instances {
		if providerID == fmt.Sprintf("gce://%s/%s/%s", inst.ProjectID, inst.Location, inst.Name) {
			return inst, nil
		}
	}
	return nil, corecloud.NewNodeClaimNotFoundError(errors.New("instance not found"))
}

func (p *lifecycleInstances) List(context.Context) ([]*instance.Instance, error) {
	return p.instances, p.err
}

func (p *lifecycleInstances) Delete(_ context.Context, providerID string) error {
	if p.err != nil {
		return p.err
	}
	p.instances = slices.DeleteFunc(p.instances, func(inst *instance.Instance) bool {
		return providerID == fmt.Sprintf("gce://%s/%s/%s", inst.ProjectID, inst.Location, inst.Name)
	})
	return nil
}

func TestGetExistingInstanceWithoutCatalogType(t *testing.T) {
	inst := &instance.Instance{
		Name: "custom-node", ProjectID: "test-project", Location: "us-central1-a",
		Type: "n2-custom-8-24576", CapacityType: karpv1.CapacityTypeOnDemand,
		CreationTime: time.Now().Add(-time.Hour),
		Labels: map[string]string{
			utils.SanitizeGCELabelValue(utils.LabelNodePoolKey): "pool",
			utils.LabelClusterLocationKey:                       "us-central1",
		},
	}
	nodePool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
		NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
	}}}}
	provider := New(
		reproClient{nodePool: nodePool, nodeClass: &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}},
		reproEvents{}, reproTypes{}, &lifecycleInstances{instances: []*instance.Instance{inst}}, nil,
	)
	ctx := karpopts.ToContext(context.Background(), &karpopts.Options{})

	got, err := provider.Get(ctx, "gce://test-project/us-central1-a/custom-node")
	require.NoError(t, err)
	require.Equal(t, "gce://test-project/us-central1-a/custom-node", got.Status.ProviderID)
	require.Equal(t, inst.Type, got.Labels[corev1.LabelInstanceTypeStable])
	require.Equal(t, inst.Location, got.Labels[corev1.LabelTopologyZone])
	require.Equal(t, inst.CapacityType, got.Labels[karpv1.CapacityTypeLabelKey])
	require.Equal(t, "pool", got.Labels[karpv1.NodePoolLabelKey])
	require.Equal(t, "us-central1", got.Labels[utils.LabelClusterLocationKey])
	require.Equal(t, inst.CreationTime, got.CreationTimestamp.Time)
	require.Empty(t, got.Status.Capacity)
}

func TestGarbageCollectionWithUnknownInstanceTypes(t *testing.T) {
	unknownOrphan := &instance.Instance{
		Name: "unknown-orphan", ProjectID: "test-project", Location: "us-central1-a",
		Type: "n2-custom-8-24576", CapacityType: karpv1.CapacityTypeOnDemand,
		CreationTime: time.Now().Add(-time.Hour),
		Labels:       map[string]string{utils.SanitizeGCELabelValue(utils.LabelNodePoolKey): "pool", utils.LabelClusterLocationKey: "us-central1"},
	}
	unknownBacked := *unknownOrphan
	unknownBacked.Name = "unknown-backed"
	knownOrphan := *unknownOrphan
	knownOrphan.Name, knownOrphan.Type = "known-orphan", "n2-standard-8"
	instances := &lifecycleInstances{instances: []*instance.Instance{unknownOrphan, &unknownBacked, &knownOrphan}}
	knownType := variantInstanceType(knownOrphan.Type, "0")
	knownType.Capacity = corev1.ResourceList{}
	knownType.Overhead = &corecloud.InstanceTypeOverhead{}
	pool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
		NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
	}}}}
	backedID := "gce://test-project/us-central1-a/unknown-backed"
	kc := lifecycleKubeClient{
		reproClient: reproClient{nodePool: pool, nodeClass: &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}},
		nodeClaims:  []karpv1.NodeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "backed"}, Status: karpv1.NodeClaimStatus{ProviderID: backedID}}},
	}
	provider := New(kc, reproEvents{}, reproTypes{variants: []*corecloud.InstanceType{knownType}}, instances, nil)
	ctx := karpopts.ToContext(context.Background(), &karpopts.Options{})

	listed, err := provider.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 3, "unknown types must not hide themselves or unrelated VMs")
	for _, nc := range listed {
		require.NotEmpty(t, nc.Labels[corev1.LabelInstanceTypeStable])
		require.Equal(t, "us-central1", nc.Labels[utils.LabelClusterLocationKey])
	}

	_, err = garbagecollection.NewController(kc, provider).Reconcile(ctx)
	require.NoError(t, err)
	remaining, err := provider.List(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "both unknown- and known-type orphans must be removed")
	require.Equal(t, backedID, remaining[0].Status.ProviderID, "a VM backed by a NodeClaim must survive")
}

type lifecycleKubeClient struct {
	reproClient
	nodeClaims []karpv1.NodeClaim
}

func (c lifecycleKubeClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	list.(*karpv1.NodeClaimList).Items = c.nodeClaims
	return nil
}

type unavailableCatalog struct {
	reproTypes
	err error
}

func (p unavailableCatalog) List(context.Context, *v1alpha1.GCENodeClass) ([]*corecloud.InstanceType, error) {
	return nil, p.err
}

type unavailableKubeClient struct {
	client.Client
	err error
}

func (c unavailableKubeClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return c.err
}

func TestExistingInstanceLookupPropagatesAPIErrors(t *testing.T) {
	apiErr := errors.New("API unavailable")
	pool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
		NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
	}}}}
	kc := reproClient{nodePool: pool, nodeClass: &v1alpha1.GCENodeClass{}}
	inst := &instance.Instance{Type: "n2-custom-8-24576", Name: "custom-node", ProjectID: "test-project", Location: "us-central1-a",
		Labels: map[string]string{utils.SanitizeGCELabelValue(utils.LabelNodePoolKey): "pool"}}
	for _, tt := range []struct {
		name        string
		kubeClient  client.Client
		catalog     unavailableCatalog
		instanceErr error
	}{
		{name: "instance API", kubeClient: kc, instanceErr: apiErr},
		{name: "catalog API", kubeClient: kc, catalog: unavailableCatalog{err: apiErr}},
		{name: "Kubernetes API", kubeClient: unavailableKubeClient{err: apiErr}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider := New(tt.kubeClient, reproEvents{}, tt.catalog, &lifecycleInstances{instances: []*instance.Instance{inst}, err: tt.instanceErr}, nil)
			ctx := karpopts.ToContext(context.Background(), &karpopts.Options{})
			got, err := provider.Get(ctx, "gce://test-project/us-central1-a/custom-node")
			require.ErrorIs(t, err, apiErr)
			require.Nil(t, got)
			listed, err := provider.List(ctx)
			require.ErrorIs(t, err, apiErr)
			require.Nil(t, listed)
		})
	}
}
