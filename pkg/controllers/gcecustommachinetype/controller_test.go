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

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/awslabs/operatorpkg/status"
	gax "github.com/googleapis/gax-go/v2"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
	containerv1 "google.golang.org/api/container/v1"
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

type fakeKubeClient struct{ client.Client }

func (f *fakeKubeClient) Status() client.SubResourceWriter { return &fakeStatusWriter{} }

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
				assert.Equal(t, metav1.ConditionUnknown, metav1.ConditionStatus(cond.Status),
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
