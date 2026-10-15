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
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

func TestPodNetworkRequestContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		multinet   bool
		rangeName  string
		override   string
		noSubnet   bool
		wantSubnet string
		wantError  bool
	}{
		{name: "selected candidate, not first configured range", multinet: true, rangeName: "chosen-pods", wantSubnet: "target-subnet"},
		{name: "primary subnet override", multinet: true, rangeName: "chosen-pods", override: "projects/host-project/regions/us-central1/subnetworks/override-subnet", wantSubnet: "override-subnet"},
		{name: "ordinary named range needs no IPAM labels", rangeName: "chosen-pods"},
		{name: "ordinary unnamed range remains unset"},
		{name: "multinet requires a named range", multinet: true, wantError: true},
		{name: "multinet requires a primary subnet", multinet: true, rangeName: "chosen-pods", noSubnet: true, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cluster := makeCluster("projects/p/global/networks/default", "regions/us-central1/subnetworks/target-subnet", "cluster-pods", false)
			cluster.NetworkConfig.EnableMultiNetworking = tt.multinet
			if tt.noSubnet {
				cluster.NetworkConfig.Subnetwork = ""
			}
			class := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"first-pods", "chosen-pods"}}}
			if tt.override != "" {
				class.Spec.NetworkConfig = &v1alpha1.NetworkConfig{Subnetwork: tt.override}
			}
			instance, err := makeProvider().buildInstance(context.Background(), onDemandNodeClaim(), class, makeNonGPUIT(),
				makeSourceMetadata("cloud.google.com/gke-np-default-subnet=source-subnet,cloud.google.com/gke-np-default-pod-range=source-pods"),
				cluster, "us-central1-a", "test-node", karpv1.CapacityTypeOnDemand, nil, 0, tt.rangeName)
			if tt.wantError {
				require.ErrorContains(t, err, "multi-networking requires a resolved primary subnet and pod secondary range")
				require.Nil(t, instance)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.rangeName, instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
			assertPodNetworkLabels(t, instance, tt.wantSubnet, tt.rangeName)
		})
	}
}

// Keep one boundary test: independently preparing two requests would not catch
// metadata being accidentally reused by the actual exhaustion retry loop.
func TestPodNetworkFallbackKeepsLabelsWithAlias(t *testing.T) {
	t.Parallel()
	var attempts []*compute.Instance
	provider := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, &compute.Operation{Name: "created", Status: "DONE"})
			return
		}
		var instance compute.Instance
		if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
			t.Errorf("decode Compute request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		attempts = append(attempts, &instance)
		if instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName == "extra-pods" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": map[string]any{"errors": []map[string]string{{"reason": "IP_SPACE_EXHAUSTED"}}}})
			return
		}
		writeJSON(w, &compute.Operation{Name: "created", Status: "DONE"})
	}))
	provider.computeDefaultSA = "node@test-project.iam.gserviceaccount.com"
	cluster := makeCluster("projects/p/global/networks/default", "regions/us-central1/subnetworks/target-subnet", "cluster-pods", false)
	cluster.NetworkConfig.EnableMultiNetworking = true
	cluster.IpAllocationPolicy.AdditionalPodRangesConfig = &containerv1.AdditionalPodRangesConfig{PodRangeNames: []string{"extra-pods"}}
	instance, _, _, err := provider.createInstanceWithPodRanges(context.Background(), onDemandNodeClaim(), &v1alpha1.GCENodeClass{}, makeNonGPUIT(),
		makeSourceMetadata("cloud.google.com/gke-np-default-subnet=source-subnet,cloud.google.com/gke-np-default-pod-range=source-pods"),
		cluster, "us-central1-a", "test-node", karpv1.CapacityTypeOnDemand, nil, 0, map[string]int64{"cluster-pods": 4096, "extra-pods": 8192})
	require.NoError(t, err)
	require.Equal(t, "cluster-pods", instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
	require.Len(t, attempts, 2)
	for i, wantRange := range []string{"extra-pods", "cluster-pods"} {
		require.Equal(t, wantRange, attempts[i].NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
		assertPodNetworkLabels(t, attempts[i], "target-subnet", wantRange)
	}
}

func assertPodNetworkLabels(t *testing.T, instance *compute.Instance, wantSubnet, wantRange string) {
	t.Helper()
	var env map[string]string
	require.NoError(t, yaml.Unmarshal([]byte(metadataValue(instance.Metadata, metadata.KubeEnvKey)), &env))
	registrationLabels := ""
	for _, arg := range strings.Fields(env["KUBELET_ARGS"]) {
		if strings.HasPrefix(arg, "--node-labels=") {
			registrationLabels = strings.TrimPrefix(arg, "--node-labels=")
		}
	}
	for _, raw := range []string{metadataValue(instance.Metadata, metadata.KubeLabelsKey), registrationLabels} {
		labels := map[string]string{}
		for _, entry := range strings.Split(raw, ",") {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				labels[key] = value
			}
		}
		if wantSubnet == "" {
			require.NotContains(t, labels, "cloud.google.com/gke-np-default-subnet")
			require.NotContains(t, labels, "cloud.google.com/gke-np-default-pod-range")
			continue
		}
		require.Equal(t, wantSubnet, labels["cloud.google.com/gke-np-default-subnet"])
		require.Equal(t, wantRange, labels["cloud.google.com/gke-np-default-pod-range"])
	}
}
