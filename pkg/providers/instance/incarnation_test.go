/*
Copyright 2026 The Karpenter GCP Authors.

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

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/utils"
)

const testClaimUID = "8d3b1a4e-1111-4222-8333-444444444444"

// createProviderDouble serves a Create launch and captures the inserted VM.
// insertResponse serves the Instances.Insert call, terminalOperation the polled
// zone operation, and postInsert any later instance read.
func createProviderDouble(t *testing.T, insertResponse func(w http.ResponseWriter), terminalOperation func(w http.ResponseWriter), postInsert http.HandlerFunc) (*DefaultProvider, func() *computeInstance) {
	t.Helper()
	var mu sync.Mutex
	var inserted *computeInstance
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/instances"):
			var request computeInstance
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			inserted = &request
			mu.Unlock()
			insertResponse(w)
		case strings.Contains(r.URL.Path, "/operations/"):
			if terminalOperation == nil {
				writeJSON(w, map[string]any{"name": "op-1", "status": "DONE"})
				return
			}
			terminalOperation(w)
		case strings.HasSuffix(r.URL.Path, "/aggregated/instances"):
			writeJSON(w, map[string]any{})
		case postInsert != nil && strings.Contains(r.URL.Path, "/instances/"):
			mu.Lock()
			created := inserted != nil
			mu.Unlock()
			if !created {
				// The pre-insert existence check must see no VM.
				http.NotFound(w, r)
				return
			}
			postInsert(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	useFakeCreateDependencies(p, "us-central1-a")
	return p, func() *computeInstance {
		mu.Lock()
		defer mu.Unlock()
		return inserted
	}
}

// computeInstance is the wire form of a Compute instance insert request.
type computeInstance struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

func createClaim(name string) *karpv1.NodeClaim {
	claim := spotOrOnDemandNodeClaim()
	claim.Name = name
	claim.UID = testClaimUID
	return claim
}

func createInstanceTypes() []*cloudprovider.InstanceType {
	return []*cloudprovider.InstanceType{spotAndOnDemandIT("n2-standard-4", 0.05, 0.2)}
}

func TestCreateRecordsTheLaunchedIncarnation(t *testing.T) {
	t.Parallel()

	p, inserted := createProviderDouble(t, func(w http.ResponseWriter) {
		writeJSON(w, map[string]any{"name": "op-1", "status": "DONE", "targetId": "1001"})
	}, nil, nil)

	instance, err := p.Create(context.Background(), imagesReadyNodeClass(), createClaim("launched"), createInstanceTypes())
	require.NoError(t, err)
	require.Equal(t, uint64(1001), instance.NumericID, "the insert operation's numeric instance ID is the trusted incarnation")
	require.Equal(t, testClaimUID, inserted().Labels[utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey)],
		"the launched VM must record which NodeClaim owns it")
}

func TestCreateDoesNotLaunchWithoutATrustedIncarnation(t *testing.T) {
	t.Parallel()

	p, _ := createProviderDouble(t, func(w http.ResponseWriter) {
		writeJSON(w, map[string]any{"name": "op-1", "status": "DONE"})
	}, nil, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})

	_, err := p.Create(context.Background(), imagesReadyNodeClass(), createClaim("unbound"), createInstanceTypes())
	require.Error(t, err, "a claim must not launch while the VM incarnation is unknown")
}

// TestCreateUsesTheTerminalOperationIncarnation guards the insert fallback: the
// completed operation reports the created VM, so a later same-name read must not
// be able to substitute a replacement VM's incarnation.
func TestCreateUsesTheTerminalOperationIncarnation(t *testing.T) {
	t.Parallel()

	postInsertReads := 0
	p, _ := createProviderDouble(t, func(w http.ResponseWriter) {
		// The insert response omits the ID.
		writeJSON(w, map[string]any{"name": "op-1", "status": "DONE"})
	}, func(w http.ResponseWriter) {
		writeJSON(w, map[string]any{"name": "op-1", "status": "DONE", "targetId": "1001"})
	}, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/instances") {
			writeJSON(w, map[string]any{})
			return
		}
		postInsertReads++
		// A same-name replacement VM without the NodeClaim's ownership label.
		writeJSON(w, map[string]any{"name": "karpenter-unbound", "id": "2002", "status": "RUNNING"})
	})

	instance, err := p.Create(context.Background(), imagesReadyNodeClass(), createClaim("unbound"), createInstanceTypes())
	require.NoError(t, err)
	require.Equal(t, uint64(1001), instance.NumericID, "the terminal operation's instance ID must win over any later read")
	require.Zero(t, postInsertReads, "a name-based read must not happen when the operation reports the ID")
}

func TestCreateReadsTheIncarnationWhenTheOperationOmitsIt(t *testing.T) {
	t.Parallel()

	p, _ := createProviderDouble(t, func(w http.ResponseWriter) {
		writeJSON(w, map[string]any{"name": "op-1", "status": "DONE"})
	}, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"name":   "karpenter-unbound",
			"id":     "2002",
			"status": "RUNNING",
			"labels": map[string]string{utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey): testClaimUID},
		})
	})

	instance, err := p.Create(context.Background(), imagesReadyNodeClass(), createClaim("unbound"), createInstanceTypes())
	require.NoError(t, err)
	require.Equal(t, uint64(2002), instance.NumericID, "the authoritative read of the owned VM supplies the incarnation")
}

// TestCreateDoesNotTrustAnUnownedFallbackInstance covers the last-resort read:
// a same-name VM that does not carry this NodeClaim's ownership label cannot
// supply the incarnation.
func TestCreateDoesNotTrustAnUnownedFallbackInstance(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		labels map[string]string
	}{
		{name: "missing ownership label"},
		{name: "another NodeClaim owns the VM", labels: map[string]string{
			utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey): "another-claim-uid",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, _ := createProviderDouble(t, func(w http.ResponseWriter) {
				writeJSON(w, map[string]any{"name": "op-1", "status": "DONE"})
			}, nil, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{
					"name":   "karpenter-unbound",
					"id":     "2002",
					"status": "RUNNING",
					"labels": tt.labels,
				})
			})

			instance, err := p.Create(context.Background(), imagesReadyNodeClass(), createClaim("unbound"), createInstanceTypes())
			require.Error(t, err, "an unowned same-name VM must not supply the launched incarnation")
			require.Nil(t, instance)
		})
	}
}

func TestAdoptRecordsTheIncarnationOnlyForTheOwningClaim(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		labels map[string]string
		wantID uint64
	}{
		{
			name:   "owning claim",
			labels: map[string]string{utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey): testClaimUID},
			wantID: 999,
		},
		{
			name:   "VM launched before the ownership label existed",
			labels: nil,
			wantID: 0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/aggregated/instances") {
					writeJSON(w, &compute.InstanceAggregatedList{Items: map[string]compute.InstancesScopedList{
						"zones/us-central1-a": {Instances: []*compute.Instance{{
							Name:   "karpenter-claim",
							Id:     999,
							Zone:   "zones/us-central1-a",
							Labels: tt.labels,
						}}},
					}})
					return
				}
				http.NotFound(w, r)
			}))

			instance, err := p.Adopt(context.Background(), createClaim("claim"))
			require.NoError(t, err)
			require.NotNil(t, instance)
			require.Equal(t, tt.wantID, instance.NumericID)
		})
	}
}

func TestAdoptRejectsAnotherNodeClaimsInstance(t *testing.T) {
	t.Parallel()

	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/aggregated/instances") {
			writeJSON(w, &compute.InstanceAggregatedList{Items: map[string]compute.InstancesScopedList{
				"zones/us-central1-a": {Instances: []*compute.Instance{{
					Name: "karpenter-claim",
					Id:   999,
					Zone: "zones/us-central1-a",
					Labels: map[string]string{
						utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey): "another-claim-uid",
					},
				}}},
			}})
			return
		}
		http.NotFound(w, r)
	}))

	instance, err := p.Adopt(context.Background(), createClaim("claim"))
	require.Error(t, err, "a VM owned by another NodeClaim must not be adopted")
	require.Nil(t, instance)
}

// TestCreateStopsWhenARegionalInstanceBelongsToAnotherClaim guards regional
// adoption: an ownership conflict is not a discovery failure that may be ignored,
// because the next selected zone would otherwise launch a duplicate VM.
func TestCreateStopsWhenARegionalInstanceBelongsToAnotherClaim(t *testing.T) {
	t.Parallel()

	inserts := 0
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/aggregated/instances"):
			writeJSON(w, &compute.InstanceAggregatedList{Items: map[string]compute.InstancesScopedList{
				"zones/us-central1-a": {Instances: []*compute.Instance{{
					Name: "karpenter-conflict",
					Id:   999,
					Zone: "zones/us-central1-a",
					Labels: map[string]string{
						utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey): "another-claim-uid",
					},
				}}},
			}})
		case r.Method == http.MethodPost:
			inserts++
			writeJSON(w, map[string]any{"name": "op-1", "status": "DONE", "targetId": "1001"})
		default:
			http.NotFound(w, r)
		}
	}))
	// Scheduling would select us-central1-b, while the conflicting VM is found by
	// the region-wide adoption lookup in us-central1-a.
	useFakeCreateDependencies(p, "us-central1-b")

	instance, err := p.Create(context.Background(), imagesReadyNodeClass(), createClaim("conflict"), createInstanceTypes())
	require.Error(t, err, "an ownership conflict must fail the create")
	require.True(t, errors.Is(err, ErrInstanceOwnershipConflict), "the conflict must stay distinguishable from transient discovery errors, got %v", err)
	require.Nil(t, instance)
	require.Zero(t, inserts, "no VM may be inserted while a same-name VM belongs to another NodeClaim")
}
