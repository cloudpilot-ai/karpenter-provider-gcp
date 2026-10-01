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
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
)

func TestCreateStopsAfterAllPodRangesAreExhausted(t *testing.T) { //nolint:gocyclo // Fake API routing covers bootstrap discovery and both exhaustion paths.
	t.Parallel()

	for _, tc := range []struct {
		name  string
		code  string
		async bool
	}{
		{name: "synchronous", code: "IP_SPACE_EXHAUSTED"},
		{name: "synchronous with details", code: "IP_SPACE_EXHAUSTED_WITH_DETAILS"},
		{name: "asynchronous", code: "IP_SPACE_EXHAUSTED", async: true},
		{name: "asynchronous with details", code: "IP_SPACE_EXHAUSTED_WITH_DETAILS", async: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var attemptedRanges, attemptedTypes []string
			p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/aggregated/instances"):
					writeJSON(w, &compute.InstanceAggregatedList{})
				case strings.Contains(r.URL.Path, "/nodePools/"):
					writeJSON(w, &containerv1.NodePool{Status: "RUNNING", InstanceGroupUrls: []string{
						"https://www.googleapis.com/compute/v1/projects/test-project/zones/us-central1-f/instanceGroupManagers/bootstrap",
					}})
				case strings.Contains(r.URL.Path, "/clusters/"):
					cluster := podRangeFallbackCluster()
					cluster.Locations = []string{"us-central1-f"}
					writeJSON(w, cluster)
				case strings.Contains(r.URL.Path, "/instanceGroupManagers/"):
					writeJSON(w, &compute.InstanceGroupManager{InstanceTemplate: "https://www.googleapis.com/compute/v1/projects/test-project/global/instanceTemplates/bootstrap"})
				case strings.Contains(r.URL.Path, "/instanceTemplates/"):
					writeJSON(w, &compute.InstanceTemplate{Properties: &compute.InstanceProperties{Metadata: makeSourceMetadata("max-pods-per-node=110")}})
				case strings.Contains(r.URL.Path, "/operations/"):
					writeJSON(w, &compute.Operation{Name: "insert", Status: "DONE", Error: &compute.OperationError{Errors: []*compute.OperationErrorErrors{{
						Code: tc.code, Message: "Pod address range is full",
					}}}})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/instances"):
					var instance compute.Instance
					if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
						t.Errorf("decoding instance: %v", err)
						http.Error(w, "invalid instance", http.StatusBadRequest)
						return
					}
					mu.Lock()
					attemptedRanges = append(attemptedRanges, instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
					attemptedTypes = append(attemptedTypes, lastPathSegment(instance.MachineType))
					mu.Unlock()
					if !tc.async {
						w.WriteHeader(http.StatusBadRequest)
						writeJSON(w, map[string]any{"error": map[string]any{"errors": []map[string]string{{"reason": tc.code, "message": "Pod address range is full"}}}})
						return
					}
					writeJSON(w, &compute.Operation{Name: "insert", Status: "RUNNING"})
				default:
					http.NotFound(w, r)
				}
			}))
			ctx := context.Background()
			containerService, err := containerv1.NewService(ctx, option.WithEndpoint(p.computeService.BasePath), option.WithoutAuthentication())
			require.NoError(t, err)
			p.gkeProvider = gke.NewDefaultProvider(p.computeService, containerService, "test-project", "us-central1", "test-cluster")
			p.nodePoolTemplateProvider = nodepooltemplate.NewDefaultProvider(ctx, p.computeService, containerService,
				"test-cluster", "us-central1", "test-project", "", "us-central1", "us-central1", "default-pool")
			require.NoError(t, p.nodePoolTemplateProvider.Sync(ctx))
			p.instanceTypeProvider = &instancetype.DefaultProvider{}
			p.computeDefaultSA = "123-compute@developer.gserviceaccount.com"
			p.unavailableOfferings = unavailableofferings.NewUnavailableOfferings()
			nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"default-pods", "extra-pods"}}}
			first := makeNonGPUIT()
			second := makeNonGPUIT()
			second.Name = "n2-standard-8"
			for i, it := range []*cloudprovider.InstanceType{first, second} {
				it.Requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"))
				it.Offerings = cloudprovider.Offerings{&cloudprovider.Offering{Available: true, Price: float64(i + 1), Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-f"),
					scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				)}}
			}

			instance, err := p.Create(ctx, nodeClass, spotOrOnDemandNodeClaim(), []*cloudprovider.InstanceType{first, second})

			require.Nil(t, instance)
			require.True(t, cloudprovider.IsInsufficientCapacityError(err), "got %v", err)
			require.ErrorContains(t, err, "IP_SPACE_EXHAUSTED")
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, []string{"default-pods", "extra-pods"}, attemptedRanges)
			require.Equal(t, []string{first.Name, first.Name}, attemptedTypes, "another machine type cannot resolve exhausted Pod ranges")
		})
	}
}
