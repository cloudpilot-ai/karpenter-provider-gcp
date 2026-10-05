/*
Copyright 2026 The Kubernetes Authors.

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
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
)

type podRangeLaunch struct {
	machineType string
	rangeName   string
}

type podRangeClusterProvider struct {
	fakeGKEProvider
}

func (*podRangeClusterProvider) GetClusterConfig(context.Context) (*containerv1.Cluster, error) {
	return podRangeFallbackCluster(), nil
}

type podRangeTemplateProvider struct {
	nodepooltemplate.Provider
}

func (*podRangeTemplateProvider) GetSourceTemplateMetadata(context.Context) (*compute.Metadata, error) {
	return makeSourceMetadata("max-pods-per-node=110"), nil
}

func newPodRangeLaunchProvider(t *testing.T, asynchronous bool, failureCode func(podRangeLaunch) string) (*DefaultProvider, func() []podRangeLaunch) {
	t.Helper()
	var mu sync.Mutex
	var attempts []podRangeLaunch
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost:
			var instance compute.Instance
			if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
				t.Errorf("decode insert: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			attempt := podRangeLaunch{
				machineType: lastPathSegment(instance.MachineType),
				rangeName:   instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName,
			}
			attempts = append(attempts, attempt)
			if code := failureCode(attempt); code != "" && !asynchronous {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"error": map[string]any{
					"errors": []map[string]string{{"reason": code, "message": "capacity exhausted"}},
				}})
				return
			}
			writeJSON(w, &compute.Operation{Name: "launch", Status: "RUNNING"})
		case strings.Contains(r.URL.Path, "/operations/"):
			op := &compute.Operation{Name: "launch", Status: "DONE"}
			if code := failureCode(attempts[len(attempts)-1]); code != "" {
				op.Error = &compute.OperationError{Errors: []*compute.OperationErrorErrors{{Code: code, Message: "capacity exhausted"}}}
			}
			writeJSON(w, op)
		case strings.Contains(r.URL.Path, "/aggregated/instances"):
			writeJSON(w, &compute.InstanceAggregatedList{})
		default:
			http.NotFound(w, r)
		}
	}))
	p.region = "us-central1"
	p.computeDefaultSA = "123-compute@developer.gserviceaccount.com"
	p.gkeProvider = &podRangeClusterProvider{fakeGKEProvider{zones: []string{"us-central1-a"}}}
	p.instanceTypeProvider = &instancetype.DefaultProvider{}
	p.nodePoolTemplateProvider = &podRangeTemplateProvider{}
	p.unavailableOfferings = unavailableofferings.NewUnavailableOfferings()
	return p, func() []podRangeLaunch {
		mu.Lock()
		defer mu.Unlock()
		return append([]podRangeLaunch(nil), attempts...)
	}
}

func podRangeLaunchInstanceTypes() []*cloudprovider.InstanceType {
	first, second := makeNonGPUIT(), makeNonGPUIT()
	second.Name = "n2-standard-8"
	for i, it := range []*cloudprovider.InstanceType{first, second} {
		it.Requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"))
		it.Offerings[0].Price = float64(i + 1)
	}
	return []*cloudprovider.InstanceType{first, second}
}

func podRangeLaunchNodeClass() *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		SubnetRangeNames: []string{"default-pods", "extra-pods"},
	}}
}

func TestCreateStopsAfterAllPodRangesExhausted(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"IP_SPACE_EXHAUSTED", "IP_SPACE_EXHAUSTED_WITH_DETAILS"} {
		for _, asynchronous := range []bool{false, true} {
			path := "insert"
			if asynchronous {
				path = "operation"
			}
			t.Run(code+"/"+path, func(t *testing.T) {
				t.Parallel()
				p, attempts := newPodRangeLaunchProvider(t, asynchronous, func(podRangeLaunch) string {
					return code
				})

				instance, err := p.Create(context.Background(), podRangeLaunchNodeClass(), onDemandNodeClaim(), podRangeLaunchInstanceTypes())

				require.Nil(t, instance)
				require.True(t, cloudprovider.IsInsufficientCapacityError(err), "got %v", err)
				require.ErrorContains(t, err, code)
				require.Equal(t, []podRangeLaunch{
					{machineType: "n2-standard-4", rangeName: "default-pods"},
					{machineType: "n2-standard-4", rangeName: "extra-pods"},
				}, attempts(), "exhausted pod ranges must not be retried with another machine type")
			})
		}
	}
}

func TestCreateFallsBackToAvailablePodRange(t *testing.T) {
	t.Parallel()
	p, attempts := newPodRangeLaunchProvider(t, true, func(attempt podRangeLaunch) string {
		if attempt.rangeName == "default-pods" {
			return "IP_SPACE_EXHAUSTED"
		}
		return ""
	})

	instance, err := p.Create(context.Background(), podRangeLaunchNodeClass(), onDemandNodeClaim(), podRangeLaunchInstanceTypes())

	require.NoError(t, err)
	require.Equal(t, "n2-standard-4", instance.Type)
	require.Equal(t, []podRangeLaunch{
		{machineType: "n2-standard-4", rangeName: "default-pods"},
		{machineType: "n2-standard-4", rangeName: "extra-pods"},
	}, attempts())
	require.False(t, p.unavailableOfferings.IsUnavailable("n2-standard-4", "us-central1-a", "on-demand"))
}

func TestCreateRetriesAnotherTypeAfterNonIPCapacityFailure(t *testing.T) {
	t.Parallel()
	p, attempts := newPodRangeLaunchProvider(t, true, func(attempt podRangeLaunch) string {
		if attempt.machineType == "n2-standard-4" {
			return "ZONE_RESOURCE_POOL_EXHAUSTED"
		}
		return ""
	})

	instance, err := p.Create(context.Background(), podRangeLaunchNodeClass(), onDemandNodeClaim(), podRangeLaunchInstanceTypes())

	require.NoError(t, err)
	require.Equal(t, "n2-standard-8", instance.Type)
	require.Equal(t, []podRangeLaunch{
		{machineType: "n2-standard-4", rangeName: "default-pods"},
		{machineType: "n2-standard-8", rangeName: "default-pods"},
	}, attempts(), "stockouts should retry another type, not another pod range")
}
