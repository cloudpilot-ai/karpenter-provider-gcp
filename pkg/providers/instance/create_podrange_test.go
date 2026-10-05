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
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	computeapi "cloud.google.com/go/compute/apiv1"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/subnet"
)

type podRangeLaunch struct {
	machineType string
	rangeName   string
}

type podRangeClusterProvider struct {
	fakeGKEProvider
	cluster *containerv1.Cluster
}

func (p *podRangeClusterProvider) GetClusterConfig(context.Context) (*containerv1.Cluster, error) {
	return p.cluster, nil
}

type podRangeTemplateProvider struct {
	nodepooltemplate.Provider
}

func (*podRangeTemplateProvider) GetSourceTemplateMetadata(context.Context) (*compute.Metadata, error) {
	return makeSourceMetadata("max-pods-per-node=110"), nil
}

func newPodRangeCapacityProvider(t *testing.T, handler http.Handler) subnet.Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := computeapi.NewSubnetworksRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return subnet.NewProvider(client, "us-central1", clock.RealClock{})
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
	p.gkeProvider = &podRangeClusterProvider{
		fakeGKEProvider: fakeGKEProvider{zones: []string{"us-central1-a"}},
		cluster:         podRangeFallbackCluster(),
	}
	p.gkeProvider.(*podRangeClusterProvider).cluster.NetworkConfig.Network = "projects/test-project/global/networks/vpc"
	p.subnetProvider = newPodRangeCapacityProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"utilizationDetails": map[string]any{"ipv4Utilizations": []map[string]string{
			{"rangeName": "default-pods", "totalFreeIp": "1000"},
			{"rangeName": "extra-pods", "totalFreeIp": "100"},
		}}})
	}))
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

func TestCreateDiscoversAdditionalPodRanges(t *testing.T) {
	t.Parallel()
	p, attempts := newPodRangeLaunchProvider(t, true, func(attempt podRangeLaunch) string {
		if attempt.rangeName == "default-pods" {
			return "IP_SPACE_EXHAUSTED"
		}
		return ""
	})

	instance, err := p.Create(context.Background(), &v1alpha1.GCENodeClass{}, onDemandNodeClaim(), podRangeLaunchInstanceTypes())

	require.NoError(t, err)
	require.Equal(t, "n2-standard-4", instance.Type)
	require.Equal(t, []podRangeLaunch{
		{machineType: "n2-standard-4", rangeName: "default-pods"},
		{machineType: "n2-standard-4", rangeName: "extra-pods"},
	}, attempts())
}

func TestCreatePodRangeOverridesReplaceClusterRanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec v1alpha1.GCENodeClassSpec
	}{
		{name: "list", spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"custom-pods"}}},
		{name: "deprecated scalar", spec: v1alpha1.GCENodeClassSpec{
			SubnetRangeName: ptr.To("custom-pods"), //nolint:staticcheck // Verify the deprecated override remains supported.
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, attempts := newPodRangeLaunchProvider(t, true, func(attempt podRangeLaunch) string {
				if attempt.rangeName == "custom-pods" {
					return "IP_SPACE_EXHAUSTED"
				}
				return ""
			})

			instance, err := p.Create(context.Background(), &v1alpha1.GCENodeClass{Spec: tt.spec}, onDemandNodeClaim(), podRangeLaunchInstanceTypes())

			require.Nil(t, instance)
			require.True(t, cloudprovider.IsInsufficientCapacityError(err), "got %v", err)
			require.Equal(t, []podRangeLaunch{{machineType: "n2-standard-4", rangeName: "custom-pods"}}, attempts(),
				"explicit overrides must not fall back to available cluster ranges")
		})
	}
}

func TestCreateSelectsMostFreeDiscoveredPodRange(t *testing.T) {
	t.Parallel()
	p, attempts := newPodRangeLaunchProvider(t, true, func(podRangeLaunch) string { return "" })
	p.subnetProvider = newPodRangeCapacityProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"utilizationDetails": map[string]any{"ipv4Utilizations": []map[string]string{
			{"rangeName": "default-pods", "totalFreeIp": "1000"},
			{"rangeName": "extra-pods", "totalFreeIp": "8000"},
		}}})
	}))

	instance, err := p.Create(context.Background(), &v1alpha1.GCENodeClass{}, onDemandNodeClaim(), podRangeLaunchInstanceTypes())

	require.NoError(t, err)
	require.Equal(t, "n2-standard-4", instance.Type)
	require.Equal(t, []podRangeLaunch{{machineType: "n2-standard-4", rangeName: "extra-pods"}}, attempts())
}

func TestCreateDiscoversConfiguredAndReportedPodRanges(t *testing.T) {
	t.Parallel()
	p, attempts := newPodRangeLaunchProvider(t, true, func(attempt podRangeLaunch) string {
		if attempt.rangeName != "unreported-pods" {
			return "IP_SPACE_EXHAUSTED"
		}
		return ""
	})
	cluster := p.gkeProvider.(*podRangeClusterProvider).cluster
	cluster.IpAllocationPolicy.AdditionalPodRangesConfig.PodRangeNames = []string{"unreported-pods"}

	instance, err := p.Create(context.Background(), &v1alpha1.GCENodeClass{}, onDemandNodeClaim(), podRangeLaunchInstanceTypes())

	require.NoError(t, err)
	require.Equal(t, "n2-standard-4", instance.Type)
	require.Equal(t, []podRangeLaunch{
		{machineType: "n2-standard-4", rangeName: "default-pods"},
		{machineType: "n2-standard-4", rangeName: "extra-pods"},
		{machineType: "n2-standard-4", rangeName: "unreported-pods"},
	}, attempts())
}

func TestCreateCapacityLookupFailureIsAdvisory(t *testing.T) {
	t.Parallel()
	p, attempts := newPodRangeLaunchProvider(t, true, func(attempt podRangeLaunch) string {
		if attempt.machineType == "n2-standard-4" {
			return "ZONE_RESOURCE_POOL_EXHAUSTED"
		}
		return ""
	})
	var lookups atomic.Int32
	p.subnetProvider = newPodRangeCapacityProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lookups.Add(1)
		http.Error(w, `{"error":{"code":403,"message":"denied"}}`, http.StatusForbidden)
	}))
	nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"extra-pods", "default-pods"}}}

	instance, err := p.Create(context.Background(), nc, onDemandNodeClaim(), podRangeLaunchInstanceTypes())

	require.NoError(t, err)
	require.Equal(t, "n2-standard-8", instance.Type)
	require.Equal(t, []podRangeLaunch{
		{machineType: "n2-standard-4", rangeName: "extra-pods"},
		{machineType: "n2-standard-8", rangeName: "extra-pods"},
	}, attempts(), "lookup failure must preserve explicit order across type retries")
	require.Equal(t, int32(1), lookups.Load(), "a failed capacity lookup must not repeat for every type")
}

func TestCreateCapacityUsesSubnetworkOverride(t *testing.T) {
	t.Parallel()
	p, attempts := newPodRangeLaunchProvider(t, true, func(podRangeLaunch) string { return "" })
	p.subnetProvider = newPodRangeCapacityProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/compute/v1/projects/host/regions/us-central1/subnetworks/custom", r.URL.Path)
		writeJSON(w, map[string]any{"utilizationDetails": map[string]any{"ipv4Utilizations": []map[string]string{
			{"rangeName": "default-pods", "totalFreeIp": "10"},
			{"rangeName": "extra-pods", "totalFreeIp": "8000"},
			{"rangeName": "ineligible", "totalFreeIp": "999999"},
		}}})
	}))
	nc := podRangeLaunchNodeClass()
	nc.Spec.NetworkConfig = &v1alpha1.NetworkConfig{Subnetwork: "projects/host/regions/us-central1/subnetworks/custom"}

	instance, err := p.Create(context.Background(), nc, onDemandNodeClaim(), podRangeLaunchInstanceTypes())

	require.NoError(t, err)
	require.Equal(t, "n2-standard-4", instance.Type)
	require.Equal(t, []podRangeLaunch{{machineType: "n2-standard-4", rangeName: "extra-pods"}}, attempts(),
		"capacity must use the target subnet without broadening eligible ranges")
}

func TestCreateZeroFreeCountStillAttemptsAllocation(t *testing.T) {
	t.Parallel()
	p, attempts := newPodRangeLaunchProvider(t, true, func(podRangeLaunch) string { return "" })
	p.subnetProvider = newPodRangeCapacityProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"utilizationDetails": map[string]any{"ipv4Utilizations": []map[string]string{
			{"rangeName": "default-pods", "totalFreeIp": "0"},
		}}})
	}))

	instance, err := p.Create(context.Background(), &v1alpha1.GCENodeClass{}, onDemandNodeClaim(), podRangeLaunchInstanceTypes())

	require.NoError(t, err)
	require.Equal(t, "n2-standard-4", instance.Type)
	require.Equal(t, []podRangeLaunch{{machineType: "n2-standard-4", rangeName: "default-pods"}}, attempts(),
		"cached zero is a hint, not a reason to skip authoritative allocation")
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
