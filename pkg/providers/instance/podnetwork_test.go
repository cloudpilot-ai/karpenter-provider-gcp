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
	"net/http/httptest"
	"strings"
	"testing"

	computeclient "cloud.google.com/go/compute/apiv1"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/subnet"
)

func TestCreateMultinetworkNodeUsesTargetPodNetwork(t *testing.T) {
	t.Parallel()
	f := newPodNetworkAPI(t)
	f.cluster.NetworkConfig.EnableMultiNetworking = true
	class := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"extra-pods"}}}
	_, err := f.create(class)
	require.NoError(t, err)
	require.Len(t, f.instances, 1)
	assertPodNetworkLabels(t, f.instances[0], "target-subnet", "extra-pods")
}

func TestCreateMultinetworkNodeDiscoversAndRanksTargetPodNetwork(t *testing.T) {
	t.Parallel()
	f := newPodNetworkAPI(t)
	f.cluster.NetworkConfig.EnableMultiNetworking = true
	f.cluster.IpAllocationPolicy.AdditionalPodRangesConfig = &containerv1.AdditionalPodRangesConfig{PodRangeNames: []string{"extra-pods"}}
	f.freeIPs = map[string]int64{"primary-pods": 1024, "extra-pods": 8192}
	_, err := f.create(&v1alpha1.GCENodeClass{})
	require.NoError(t, err)
	require.Len(t, f.instances, 1)
	assertPodNetworkLabels(t, f.instances[0], "target-subnet", "extra-pods")
}

func TestCreateMultinetworkNodeUpdatesPodNetworkAfterIPExhaustion(t *testing.T) {
	t.Parallel()
	f := newPodNetworkAPI(t)
	f.cluster.NetworkConfig.EnableMultiNetworking = true
	f.rejectedRange = "first-pods"
	_, err := f.create(&v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{SubnetRangeNames: []string{"first-pods", "second-pods"}}})
	require.NoError(t, err)
	require.Len(t, f.instances, 2)
	assertPodNetworkLabels(t, f.instances[0], "target-subnet", "first-pods")
	assertPodNetworkLabels(t, f.instances[1], "target-subnet", "second-pods")
	require.Equal(t, f.instances[0].Name, f.instances[1].Name)
}

func TestCreateOrdinaryNodePreservesUnnamedPodRangeFallback(t *testing.T) {
	t.Parallel()
	f := newPodNetworkAPI(t)
	f.cluster.IpAllocationPolicy = nil
	_, err := f.create(&v1alpha1.GCENodeClass{})
	require.NoError(t, err)
	require.Len(t, f.instances, 1)
	require.Empty(t, f.instances[0].NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
	for _, labels := range []map[string]string{parseLabelString(kubeLabelsFrom(t, f.instances[0])), nodeLabelsFromKubeEnv(kubeEnvFrom(t, f.instances[0]))} {
		require.NotContains(t, labels, "cloud.google.com/gke-np-default-subnet")
		require.NotContains(t, labels, "cloud.google.com/gke-np-default-pod-range")
	}
}

func TestCreateOrdinaryNodeDiscoversPodRangesWithoutIPAMLabels(t *testing.T) {
	t.Parallel()
	f := newPodNetworkAPI(t)
	f.cluster.IpAllocationPolicy.AdditionalPodRangesConfig = &containerv1.AdditionalPodRangesConfig{PodRangeNames: []string{"extra-pods"}}
	f.freeIPs = map[string]int64{"primary-pods": 1024, "extra-pods": 8192}
	_, err := f.create(&v1alpha1.GCENodeClass{})
	require.NoError(t, err)
	require.Len(t, f.instances, 1)
	require.Equal(t, "extra-pods", f.instances[0].NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
	for _, labels := range []map[string]string{parseLabelString(kubeLabelsFrom(t, f.instances[0])), nodeLabelsFromKubeEnv(kubeEnvFrom(t, f.instances[0]))} {
		require.NotContains(t, labels, "cloud.google.com/gke-np-default-subnet")
		require.NotContains(t, labels, "cloud.google.com/gke-np-default-pod-range")
	}
}

func TestCreateMultinetworkNodeRejectsUnresolvedPodRangeBeforeInsert(t *testing.T) {
	t.Parallel()
	f := newPodNetworkAPI(t)
	f.cluster.NetworkConfig.EnableMultiNetworking = true
	f.cluster.IpAllocationPolicy = nil
	_, err := f.create(&v1alpha1.GCENodeClass{})
	require.ErrorContains(t, err, "multi-networking requires a resolved primary subnet and pod secondary range")
	require.Empty(t, f.instances)
}

func TestCreateAdoptsExistingNodeBeforeValidatingPodNetwork(t *testing.T) {
	t.Parallel()
	f := newPodNetworkAPI(t)
	f.cluster.NetworkConfig.EnableMultiNetworking = true
	f.cluster.IpAllocationPolicy = nil
	f.existing = &compute.Instance{Name: "karpenter-test-claim", Zone: "zones/us-central1-a", MachineType: "zones/us-central1-a/machineTypes/n2-standard-4", Status: "RUNNING"}
	instance, err := f.create(&v1alpha1.GCENodeClass{})
	require.NoError(t, err)
	require.Equal(t, f.existing.Name, instance.Name)
	require.Empty(t, f.instances)
}

func TestCreateMultinetworkNodeUsesPrimarySubnetOverride(t *testing.T) {
	t.Parallel()
	f := newPodNetworkAPI(t)
	f.cluster.NetworkConfig.EnableMultiNetworking = true
	class := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		SubnetRangeNames: []string{"extra-pods"},
		NetworkConfig:    &v1alpha1.NetworkConfig{Subnetwork: "projects/host-project/regions/us-central1/subnetworks/override-subnet"},
	}}
	_, err := f.create(class)
	require.NoError(t, err)
	require.Len(t, f.instances, 1)
	assertPodNetworkLabels(t, f.instances[0], "override-subnet", "extra-pods")
}

// All collaborators are real providers. Only the GKE/Compute HTTP boundary is replaced.
type podNetworkAPI struct {
	provider      Provider
	cluster       *containerv1.Cluster
	instances     []*compute.Instance
	rejectedRange string
	existing      *compute.Instance
	freeIPs       map[string]int64
}

func newPodNetworkAPI(t *testing.T) *podNetworkAPI {
	t.Helper()
	f := &podNetworkAPI{cluster: makeCluster("projects/test-project/global/networks/default", "regions/us-central1/subnetworks/target-subnet", "primary-pods", false), freeIPs: map[string]int64{}}
	f.cluster.Locations = []string{"us-central1-a"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet:
			f.serveGet(t, w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/instances"):
			var instance compute.Instance
			if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
				t.Errorf("decode Compute Insert: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.instances = append(f.instances, &instance)
			if instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName == f.rejectedRange && f.rejectedRange != "" {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"error": map[string]any{"errors": []map[string]string{{"reason": "IP_SPACE_EXHAUSTED"}}}})
				return
			}
			writeJSON(w, &compute.Operation{Name: "created", Status: "DONE"})
		default:
			t.Errorf("unexpected GCP request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	computeService, err := compute.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	containerService, err := containerv1.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	subnetClient, err := computeclient.NewSubnetworksRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, subnetClient.Close()) })
	gkeProvider := gke.NewDefaultProvider(computeService, containerService, "test-project", "us-central1", "test-cluster")
	templates := nodepooltemplate.NewDefaultProvider(context.Background(), computeService, containerService, "test-cluster", "us-central1", "test-project", "", "us-central1", "us-central1", "bootstrap")
	require.NoError(t, templates.Sync(context.Background()))
	f.provider = NewProvider("test-cluster", "us-central1", "us-central1", "test-project", "node@test-project.iam.gserviceaccount.com", "", computeService,
		gkeProvider, subnet.NewProvider(subnetClient, "us-central1", clock.RealClock{}), &instancetype.DefaultProvider{}, templates, nil, unavailableofferings.NewUnavailableOfferings())
	return f
}

func (f *podNetworkAPI) serveGet(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	switch path := r.URL.Path; {
	case strings.HasSuffix(path, "/clusters/test-cluster"):
		writeJSON(w, f.cluster)
	case strings.HasSuffix(path, "/nodePools/bootstrap"):
		writeJSON(w, &containerv1.NodePool{Name: "bootstrap", Status: "RUNNING", InstanceGroupUrls: []string{"https://www.googleapis.com/compute/v1/projects/test-project/zones/us-central1-a/instanceGroupManagers/bootstrap"}})
	case strings.HasSuffix(path, "/instanceGroupManagers/bootstrap"):
		writeJSON(w, &compute.InstanceGroupManager{InstanceTemplate: "https://www.googleapis.com/compute/v1/projects/test-project/global/instanceTemplates/bootstrap"})
	case strings.HasSuffix(path, "/instanceTemplates/bootstrap"):
		writeJSON(w, &compute.InstanceTemplate{Name: "bootstrap", Properties: &compute.InstanceProperties{Metadata: makeSourceMetadata("cloud.google.com/gke-np-default-subnet=source-subnet,cloud.google.com/gke-np-default-pod-range=source-pods")}})
	case strings.Contains(path, "/aggregated/instances"):
		f.serveInstances(w)
	case strings.Contains(path, "/instances/"):
		if f.existing != nil {
			writeJSON(w, f.existing)
		} else {
			http.NotFound(w, r)
		}
	case strings.Contains(path, "/subnetworks/"):
		var ranges []map[string]any
		for name, count := range f.freeIPs {
			ranges = append(ranges, map[string]any{"rangeName": name, "totalFreeIp": count})
		}
		writeJSON(w, map[string]any{"utilizationDetails": map[string]any{"ipv4Utilizations": ranges}})
	case strings.Contains(path, "/operations/"):
		writeJSON(w, &compute.Operation{Name: "created", Status: "DONE"})
	default:
		t.Errorf("unexpected GCP GET: %s", r.URL)
		http.NotFound(w, r)
	}
}

func (f *podNetworkAPI) serveInstances(w http.ResponseWriter) {
	items := map[string]compute.InstancesScopedList{}
	if f.existing != nil {
		items["zones/us-central1-a"] = compute.InstancesScopedList{Instances: []*compute.Instance{f.existing}}
	}
	writeJSON(w, &compute.InstanceAggregatedList{Items: items})
}

func (f *podNetworkAPI) create(class *v1alpha1.GCENodeClass) (*Instance, error) {
	claim := onDemandNodeClaim()
	claim.Name = "test-claim"
	it := makeNonGPUIT()
	it.Requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"))
	it.Offerings[0].Requirements.Add(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"))
	return f.provider.Create(context.Background(), class, claim, []*cloudprovider.InstanceType{it})
}

func assertPodNetworkLabels(t *testing.T, instance *compute.Instance, wantSubnet, wantRange string) {
	t.Helper()
	require.Equal(t, wantRange, instance.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName)
	for _, labels := range []map[string]string{parseLabelString(kubeLabelsFrom(t, instance)), nodeLabelsFromKubeEnv(kubeEnvFrom(t, instance))} {
		require.Equal(t, wantSubnet, labels["cloud.google.com/gke-np-default-subnet"])
		require.Equal(t, wantRange, labels["cloud.google.com/gke-np-default-pod-range"])
	}
}
