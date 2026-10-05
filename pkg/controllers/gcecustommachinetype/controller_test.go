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

package gcecustommachinetype

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/awslabs/operatorpkg/status"
	gax "github.com/googleapis/gax-go/v2"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	containerv1 "google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
)

// fakeGKEProvider returns a fixed zone list without making GCP API calls.
type fakeGKEProvider struct {
	zones []string
}

func (f *fakeGKEProvider) ResolveClusterZones(_ context.Context) ([]string, error) {
	return f.zones, nil
}

func (f *fakeGKEProvider) GetClusterConfig(_ context.Context) (*containerv1.Cluster, error) {
	return &containerv1.Cluster{}, nil
}

func (f *fakeGKEProvider) GetServerConfig(_ context.Context) (*containerv1.ServerConfig, error) {
	return &containerv1.ServerConfig{}, nil
}

// fakeStatusWriter no-ops Patch: the object Reconcile receives and mutates is the same pointer
// the test inspects afterward, so persisting it isn't needed to observe the result.
type fakeStatusWriter struct{ client.SubResourceWriter }

func (f *fakeStatusWriter) Patch(_ context.Context, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
	return nil
}

// fakeKubeClient serves a fixed set of other GCECustomMachineType objects to List calls, for
// exercising duplicate-registration detection without a live API server.
type fakeKubeClient struct {
	client.Client
	others []v1alpha1.GCECustomMachineType
}

func (f *fakeKubeClient) Status() client.SubResourceWriter { return &fakeStatusWriter{} }

func (f *fakeKubeClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if v, ok := list.(*v1alpha1.GCECustomMachineTypeList); ok {
		v.Items = f.others
	}
	return nil
}

func TestGCECustomMachineTypeReconcile(t *testing.T) {
	tests := []struct {
		name           string
		zones          []string
		getMachineType func(zone string) (*computepb.MachineType, error)
		wantReady      bool
		wantErr        bool
		wantZones      []string
		wantGuestCpus  int32
	}{
		{
			name:  "resolves in all zones",
			zones: []string{"us-central1-a", "us-central1-b"},
			getMachineType: func(_ string) (*computepb.MachineType, error) {
				return &computepb.MachineType{GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](24576)}, nil
			},
			wantReady:     true,
			wantZones:     []string{"us-central1-a", "us-central1-b"},
			wantGuestCpus: 8,
		},
		{
			name:  "not found in any zone",
			zones: []string{"us-central1-a"},
			getMachineType: func(_ string) (*computepb.MachineType, error) {
				return nil, &googleapi.Error{Code: http.StatusNotFound}
			},
			wantReady: false,
			wantZones: nil,
		},
		{
			name:  "partial availability",
			zones: []string{"us-central1-a", "us-central1-b"},
			getMachineType: func(zone string) (*computepb.MachineType, error) {
				if zone == "us-central1-b" {
					return nil, &googleapi.Error{Code: http.StatusNotFound}
				}
				return &computepb.MachineType{GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](24576)}, nil
			},
			wantReady:     true,
			wantZones:     []string{"us-central1-a"},
			wantGuestCpus: 8,
		},
		{
			name:  "transient error in every zone returns error, does not set a condition",
			zones: []string{"us-central1-a"},
			getMachineType: func(_ string) (*computepb.MachineType, error) {
				return nil, errors.New("rpc error: internal")
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &v1alpha1.GCECustomMachineType{
				ObjectMeta: metav1.ObjectMeta{Name: "n2-custom-8-24576"},
				Spec: v1alpha1.GCECustomMachineTypeSpec{
					MachineType: "n2-custom-8-24576",
					Prices:      v1alpha1.GCECustomMachineTypePrices{OnDemand: "0.40", Spot: "0.12"},
				},
			}

			c := &Controller{
				kubeClient:  &fakeKubeClient{},
				authOptions: &auth.Credential{ProjectID: "test-project"},
				gkeProvider: &fakeGKEProvider{zones: tt.zones},
				getMachineType: func(_ context.Context, req *computepb.GetMachineTypeRequest, _ ...gax.CallOption) (*computepb.MachineType, error) {
					return tt.getMachineType(req.GetZone())
				},
			}

			_, err := c.Reconcile(context.Background(), obj)
			if tt.wantErr {
				require.Error(t, err)
				cond := obj.StatusConditions().Get(status.ConditionReady)
				assert.Equal(t, metav1.ConditionUnknown, cond.Status,
					"a fully-transient failure must leave Ready at its untouched default (Unknown), never False")
				return
			}
			require.NoError(t, err)

			cond := obj.StatusConditions().Get(status.ConditionReady)
			require.NotNil(t, cond)
			assert.Equal(t, tt.wantReady, cond.IsTrue())
			assert.ElementsMatch(t, tt.wantZones, obj.Status.Zones)
			if tt.wantReady {
				assert.Equal(t, tt.wantGuestCpus, obj.Status.GuestCpus)
			}
		})
	}
}

// TestGCECustomMachineTypeReconcile_PartialFailureRetriesPromptly is a regression test for a
// Greptile review finding on PR #601: a zone that resolves alongside another that hits a
// transient (non-404) error must still be persisted (so confirmed-good capacity isn't lost),
// but the object must be requeued for a prompt retry rather than waiting the full resolveTTL,
// so the unresolved zone isn't left out of the catalog for up to an hour.
func TestGCECustomMachineTypeReconcile_PartialFailureRetriesPromptly(t *testing.T) {
	obj := &v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: "n2-custom-8-24576"},
		Spec: v1alpha1.GCECustomMachineTypeSpec{
			MachineType: "n2-custom-8-24576",
			Prices:      v1alpha1.GCECustomMachineTypePrices{OnDemand: "0.40", Spot: "0.12"},
		},
	}

	c := &Controller{
		kubeClient:  &fakeKubeClient{},
		authOptions: &auth.Credential{ProjectID: "test-project"},
		gkeProvider: &fakeGKEProvider{zones: []string{"us-central1-a", "us-central1-b"}},
		getMachineType: func(_ context.Context, req *computepb.GetMachineTypeRequest, _ ...gax.CallOption) (*computepb.MachineType, error) {
			if req.GetZone() == "us-central1-b" {
				return nil, errors.New("rpc error: internal")
			}
			return &computepb.MachineType{GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](24576)}, nil
		},
	}

	_, err := c.Reconcile(context.Background(), obj)
	require.Error(t, err, "a still-unresolved zone must trigger a prompt retry, not the long resolveTTL")

	cond := obj.StatusConditions().Get(status.ConditionReady)
	require.NotNil(t, cond)
	assert.True(t, cond.IsTrue(), "the confirmed-good zone must still be persisted as Ready")
	assert.Equal(t, []string{"us-central1-a"}, obj.Status.Zones)
}

func TestGCECustomMachineTypeReconcile_RemovesLastConfirmedZoneDuringPartialFailure(t *testing.T) {
	obj := &v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: "custom"},
		Spec:       v1alpha1.GCECustomMachineTypeSpec{MachineType: "n2-custom-8-24576"},
		Status:     v1alpha1.GCECustomMachineTypeStatus{GuestCpus: 8, MemoryMb: 24576, Zones: []string{"us-central1-a"}},
	}
	obj.StatusConditions().SetTrue(status.ConditionReady)
	c := &Controller{
		kubeClient:  &fakeKubeClient{},
		authOptions: &auth.Credential{ProjectID: "test-project"},
		gkeProvider: &fakeGKEProvider{zones: []string{"us-central1-a", "us-central1-b"}},
		getMachineType: func(_ context.Context, req *computepb.GetMachineTypeRequest, _ ...gax.CallOption) (*computepb.MachineType, error) {
			if req.GetZone() == "us-central1-a" {
				return nil, &googleapi.Error{Code: http.StatusNotFound}
			}
			return nil, errors.New("temporary API failure")
		},
	}
	_, err := c.Reconcile(context.Background(), obj)
	require.Error(t, err)
	assert.Empty(t, obj.Status.Zones)
	assert.Equal(t, metav1.ConditionUnknown, obj.StatusConditions().Get(status.ConditionReady).Status)
}

func TestGCECustomMachineTypeReconcile_RefreshConfirmedZones(t *testing.T) {
	for _, tt := range []struct {
		name      string
		zones     []string
		errors    map[string]error
		wantZones []string
		wantErr   bool
	}{
		{
			name:      "partial transient failure retains confirmed zone",
			zones:     []string{"us-central1-a", "us-central1-b"},
			errors:    map[string]error{"us-central1-b": errors.New("temporary API failure")},
			wantZones: []string{"us-central1-a", "us-central1-b"},
			wantErr:   true,
		},
		{
			name:      "all transient failures retain confirmed resources",
			zones:     []string{"us-central1-a", "us-central1-b"},
			errors:    map[string]error{"us-central1-a": errors.New("temporary API failure"), "us-central1-b": errors.New("temporary API failure")},
			wantZones: []string{"us-central1-a", "us-central1-b"},
			wantErr:   true,
		},
		{
			name:      "definitive absence removes zone even alongside transient failure",
			zones:     []string{"us-central1-a", "us-central1-b"},
			errors:    map[string]error{"us-central1-a": errors.New("temporary API failure"), "us-central1-b": &googleapi.Error{Code: http.StatusNotFound}},
			wantZones: []string{"us-central1-a"},
			wantErr:   true,
		},
		{
			name:      "zone removed from cluster is not retained",
			zones:     []string{"us-central1-a"},
			errors:    map[string]error{"us-central1-a": errors.New("temporary API failure")},
			wantZones: []string{"us-central1-a"},
			wantErr:   true,
		},
		{
			name:      "definitive absence removes zone after successful lookup",
			zones:     []string{"us-central1-a", "us-central1-b"},
			errors:    map[string]error{"us-central1-b": &googleapi.Error{Code: http.StatusNotFound}},
			wantZones: []string{"us-central1-a"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := &v1alpha1.GCECustomMachineType{
				ObjectMeta: metav1.ObjectMeta{Name: "custom"},
				Spec:       v1alpha1.GCECustomMachineTypeSpec{MachineType: "n2-custom-8-24576"},
				Status: v1alpha1.GCECustomMachineTypeStatus{
					GuestCpus: 8,
					MemoryMb:  24576,
					Zones:     []string{"us-central1-a", "us-central1-b"},
				},
			}
			obj.StatusConditions().SetTrue(status.ConditionReady)
			c := &Controller{
				kubeClient:  &fakeKubeClient{},
				authOptions: &auth.Credential{ProjectID: "test-project"},
				gkeProvider: &fakeGKEProvider{zones: tt.zones},
				getMachineType: func(_ context.Context, req *computepb.GetMachineTypeRequest, _ ...gax.CallOption) (*computepb.MachineType, error) {
					if err := tt.errors[req.GetZone()]; err != nil {
						return nil, err
					}
					return &computepb.MachineType{GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](24576)}, nil
				},
			}

			_, err := c.Reconcile(context.Background(), obj)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.True(t, obj.StatusConditions().Get(status.ConditionReady).IsTrue())
			assert.ElementsMatch(t, tt.wantZones, obj.Status.Zones)
			assert.Equal(t, int32(8), obj.Status.GuestCpus)
			assert.Equal(t, int32(24576), obj.Status.MemoryMb)
		})
	}
}

// TestGCECustomMachineTypeReconcile_DuplicateRegistration is a regression test for a Greptile
// review finding on PR #601: two GCECustomMachineType objects can name the same
// spec.machineType, and without an explicit tie-break the catalog merge would silently use
// whichever one's price happened to be indexed last. The outranked object (here, the later of
// two otherwise-identical registrations) must be set Ready=False instead of resolving normally.
func TestGCECustomMachineTypeReconcile_DuplicateRegistration(t *testing.T) {
	earlier := v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: "first", CreationTimestamp: metav1.NewTime(metav1.Now().Add(-time.Hour))},
		Spec: v1alpha1.GCECustomMachineTypeSpec{
			MachineType: "n2-custom-8-24576",
			Prices:      v1alpha1.GCECustomMachineTypePrices{OnDemand: "0.40", Spot: "0.12"},
		},
	}
	later := v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: "second", CreationTimestamp: metav1.Now()},
		Spec: v1alpha1.GCECustomMachineTypeSpec{
			MachineType: "n2-custom-8-24576",
			Prices:      v1alpha1.GCECustomMachineTypePrices{OnDemand: "0.99", Spot: "0.30"},
		},
	}

	calledGetMachineType := false
	c := &Controller{
		kubeClient:  &fakeKubeClient{others: []v1alpha1.GCECustomMachineType{earlier, later}},
		authOptions: &auth.Credential{ProjectID: "test-project"},
		gkeProvider: &fakeGKEProvider{zones: []string{"us-central1-a"}},
		getMachineType: func(context.Context, *computepb.GetMachineTypeRequest, ...gax.CallOption) (*computepb.MachineType, error) {
			calledGetMachineType = true
			return &computepb.MachineType{GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](24576)}, nil
		},
	}

	loser := later.DeepCopy()
	_, err := c.Reconcile(context.Background(), loser)
	require.NoError(t, err)
	cond := loser.StatusConditions().Get(status.ConditionReady)
	require.NotNil(t, cond)
	assert.False(t, cond.IsTrue(), "the outranked (later-created) duplicate must not become Ready")
	assert.Equal(t, "MachineTypeAlreadyRegistered", cond.Reason)
	assert.False(t, calledGetMachineType, "an outranked duplicate should short-circuit before ever calling GCE")

	winner := earlier.DeepCopy()
	_, err = c.Reconcile(context.Background(), winner)
	require.NoError(t, err)
	cond = winner.StatusConditions().Get(status.ConditionReady)
	require.NotNil(t, cond)
	assert.True(t, cond.IsTrue(), "the higher-ranked (earlier-created) registration must resolve normally")
}

// TestEnqueueSiblingsWithSameMachineType is a regression test for a Greptile review finding on
// PR #601: when the winning GCECustomMachineType registration for a machine type is deleted, a
// duplicate previously outranked (and left Ready=False) must be requeued immediately, not left
// to wait out its own resolveTTL, or the shape goes unschedulable for up to an hour.
func TestEnqueueSiblingsWithSameMachineType(t *testing.T) {
	deletedWinner := &v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: "winner"},
		Spec:       v1alpha1.GCECustomMachineTypeSpec{MachineType: "n2-custom-8-24576"},
	}
	duplicate := v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: "duplicate"},
		Spec:       v1alpha1.GCECustomMachineTypeSpec{MachineType: "n2-custom-8-24576"},
	}
	unrelated := v1alpha1.GCECustomMachineType{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated"},
		Spec:       v1alpha1.GCECustomMachineTypeSpec{MachineType: "n2-custom-4-8192"},
	}

	c := &Controller{
		kubeClient: &fakeKubeClient{others: []v1alpha1.GCECustomMachineType{duplicate, unrelated}},
	}

	requests := c.enqueueSiblingsWithSameMachineType(context.Background(), deletedWinner)
	require.Len(t, requests, 1, "only the same-machineType duplicate should be requeued, not the deleted object itself or an unrelated registration")
	assert.Equal(t, "duplicate", requests[0].Name)
}

func (*fakeGKEProvider) InvalidateClusterConfig() {}
