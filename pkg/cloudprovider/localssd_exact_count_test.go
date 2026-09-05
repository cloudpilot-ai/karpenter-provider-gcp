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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/container/v1"
	googleoption "google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloud "sigs.k8s.io/karpenter/pkg/cloudprovider"
	corescheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	gcpoptions "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/version"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/localssd"
)

type reproEvents struct{}

func (reproEvents) Publish(...events.Event) {}

type reproClient struct {
	client.Client
	nodeClass         *v1alpha1.GCENodeClass
	nodePool          *karpv1.NodePool
	nodeClassNotFound bool
	nodePoolNotFound  bool
}

func (c reproClient) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	switch out := obj.(type) {
	case *v1alpha1.GCENodeClass:
		if c.nodeClassNotFound {
			return apierrors.NewNotFound(schema.GroupResource{Group: "karpenter.k8s.gcp", Resource: "gcenodeclasses"}, "default")
		}
		*out = *c.nodeClass.DeepCopy()
		return nil
	case *karpv1.NodePool:
		if c.nodePoolNotFound {
			return apierrors.NewNotFound(schema.GroupResource{Group: "karpenter.sh", Resource: "nodepools"}, "pool")
		}
		if c.nodePool == nil {
			return errors.New("repro NodePool is not configured")
		}
		*out = *c.nodePool.DeepCopy()
		return nil
	default:
		return errors.New("unsupported repro object")
	}
}

type reproTypes struct {
	variants []*corecloud.InstanceType
	machine  *computepb.MachineType
}

func (p reproTypes) LivenessProbe(*http.Request) error { return nil }
func (p reproTypes) List(context.Context, *v1alpha1.GCENodeClass) ([]*corecloud.InstanceType, error) {
	return p.variants, nil
}
func (p reproTypes) UpdateInstanceTypes(context.Context) error         { return nil }
func (p reproTypes) UpdateInstanceTypeOfferings(context.Context) error { return nil }
func (p reproTypes) GetMachineType(string) *computepb.MachineType      { return p.machine }

type reproGKE struct{}

func (reproGKE) ResolveClusterZones(context.Context) ([]string, error) {
	return []string{"us-central1-a"}, nil
}
func (reproGKE) GetClusterConfig(context.Context) (*container.Cluster, error) {
	return &container.Cluster{Id: "deadbeef", NetworkConfig: &container.NetworkConfig{Network: "projects/test/global/networks/default", Subnetwork: "projects/test/regions/us-central1/subnetworks/default"}}, nil
}
func (reproGKE) GetServerConfig(context.Context) (*container.ServerConfig, error) {
	return &container.ServerConfig{}, nil
}

type reproTemplate struct{}

func (reproTemplate) Sync(context.Context) error               { return nil }
func (reproTemplate) EnsureFallbackPool(context.Context) error { return nil }
func (reproTemplate) GetSourceTemplateMetadata(context.Context) (*compute.Metadata, error) {
	return &compute.Metadata{}, nil
}

type reproVersion struct{}

func (reproVersion) Get(context.Context) (string, error) { return "1.35.0", nil }

var _ instancetype.Provider = reproTypes{}
var _ gke.Provider = reproGKE{}
var _ nodepooltemplate.Provider = reproTemplate{}
var _ version.Provider = reproVersion{}

func TestExactCountCreateMatrix(t *testing.T) {
	fullCatalog := append([]int{0}, localssd.AllowedLocalSSDCounts("n2d-standard-4", 4)...)
	countRequirement := func(operator corev1.NodeSelectorOperator, values ...string) []karpv1.NodeSelectorRequirementWithMinValues {
		return []karpv1.NodeSelectorRequirementWithMinValues{{
			Key: v1alpha1.LabelInstanceLocalSsdCount, Operator: operator, Values: values,
		}}
	}
	cases := []struct {
		name                     string
		poolReq                  []karpv1.NodeSelectorRequirementWithMinValues
		podCount                 string
		daemonCount              string
		wantCounts               []string
		wantSerialized           string
		wantInsert               string
		unschedulable            bool
		wantInsufficientCapacity bool
	}{
		{name: "pool Gt zero and pod exact four", poolReq: countRequirement(corev1.NodeSelectorOpGt, "0"), podCount: "4", daemonCount: "2", wantCounts: []string{"4"}, wantSerialized: "4", wantInsert: "4"},
		{name: "pool Exists and pod exact zero", poolReq: countRequirement(corev1.NodeSelectorOpExists), podCount: "0", wantCounts: []string{"0"}, wantSerialized: "0", wantInsert: "0"},
		{name: "pool Exists and pod exact four", poolReq: countRequirement(corev1.NodeSelectorOpExists), podCount: "4", wantCounts: []string{"4"}, wantSerialized: "4", wantInsert: "4"},
		{name: "pool Exists and unpinned pod is rejected", poolReq: countRequirement(corev1.NodeSelectorOpExists), wantCounts: []string{"0", "1", "2", "4", "8", "16", "24"}, wantInsufficientCapacity: true},
		{name: "pool singleton four", poolReq: countRequirement(corev1.NodeSelectorOpIn, "4"), daemonCount: "2", wantCounts: []string{"4"}, wantSerialized: "4", wantInsert: "4"},
		{name: "pool two counts and pod exact two", poolReq: countRequirement(corev1.NodeSelectorOpIn, "2", "4"), podCount: "2", wantCounts: []string{"2"}, wantSerialized: "2", wantInsert: "2"},
		{name: "pool two counts and pod exact four", poolReq: countRequirement(corev1.NodeSelectorOpIn, "2", "4"), podCount: "4", daemonCount: "2", wantCounts: []string{"4"}, wantSerialized: "4", wantInsert: "4"},
		{name: "pool two counts and pod outside set", poolReq: countRequirement(corev1.NodeSelectorOpIn, "2", "4"), podCount: "8", unschedulable: true},
		{name: "broad positive-only set is rejected", poolReq: countRequirement(corev1.NodeSelectorOpIn, "2", "4"), wantCounts: []string{"2", "4"}, wantSerialized: "2,4", wantInsufficientCapacity: true},
		{name: "broad Gt zero is rejected", poolReq: countRequirement(corev1.NodeSelectorOpGt, "0"), wantCounts: []string{"1", "2", "4", "8", "16", "24"}, wantSerialized: "1", wantInsufficientCapacity: true},
		{name: "multi-value pool and pod exact zero", poolReq: countRequirement(corev1.NodeSelectorOpIn, "0", "2", "4"), podCount: "0", wantCounts: []string{"0"}, wantSerialized: "0", wantInsert: "0"},
		{name: "broad set including zero is rejected", poolReq: countRequirement(corev1.NodeSelectorOpIn, "0", "2", "4"), wantCounts: []string{"0", "2", "4"}, wantSerialized: "0,2,4", wantInsufficientCapacity: true},
		{name: "keyless pool defaults to zero", wantCounts: []string{"0"}, wantInsert: "0"},
		{name: "keyless pool accounts for count zero daemon", daemonCount: "0", unschedulable: true},
		{name: "keyless pool does not opt into positive count", podCount: "4", unschedulable: true},
	}

	for _, mode := range []v1alpha1.LocalSSDMode{v1alpha1.LocalSSDModeRawBlock, v1alpha1.LocalSSDModeEphemeral} {
		t.Run(string(mode), func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					got := runCreateScenario(t, createConfig{
						mode: mode, poolReq: tc.poolReq, podCount: tc.podCount, daemonCount: tc.daemonCount,
						ssdCounts: fullCatalog, cpuMatrix: true, useProviderMenu: true,
					})
					require.Equal(t, got.originalCapacity, got.capacity, "scheduling and Create must not mutate factory capacities")
					if tc.unschedulable {
						require.Error(t, got.appError)
						require.Zero(t, got.newNodeClaims)
						require.Zero(t, got.insertAttempts)
						require.NoError(t, got.createError)
						return
					}

					require.NoError(t, got.appError)
					require.Equal(t, 1, got.newNodeClaims)
					require.Equal(t, tc.wantCounts, got.schedulerCounts)
					require.Equal(t, tc.wantSerialized, got.serializedCount)
					if tc.wantInsufficientCapacity {
						require.True(t, corecloud.IsInsufficientCapacityError(got.createError))
						require.Contains(t, got.createError.Error(), v1alpha1.LabelInstanceLocalSsdCount)
						require.Zero(t, got.insertAttempts)
						require.Empty(t, got.insertedCount)
						return
					}

					require.NoError(t, got.createError)
					require.Equal(t, tc.wantInsert, got.insertedCount)
					require.Equal(t, got.insertedCount, strconv.Itoa(got.scratchDisks))
					require.Equal(t, 1, got.insertAttempts)
				})
			}
		})
	}
}

func TestEphemeralStorageWithoutExactCountDoesNotSelectPositiveCount(t *testing.T) {
	counts := append([]int{0}, localssd.AllowedLocalSSDCounts("n2d-standard-4", 4)...)
	got := runCreateScenario(t, createConfig{
		mode: v1alpha1.LocalSSDModeEphemeral,
		poolReq: []karpv1.NodeSelectorRequirementWithMinValues{{
			Key: v1alpha1.LabelInstanceLocalSsdCount, Operator: corev1.NodeSelectorOpIn, Values: []string{"0", "2", "4"},
		}},
		ssdCounts: counts, ephemeralGiB: 800, useProviderMenu: true,
	})
	require.NoError(t, got.appError)
	require.Equal(t, []string{"4"}, got.schedulerCounts)
	require.True(t, corecloud.IsInsufficientCapacityError(got.createError))
	require.Contains(t, got.createError.Error(), v1alpha1.LabelInstanceLocalSsdCount)
	require.Zero(t, got.insertAttempts, "ephemeral-storage capacity alone must not select a positive count")
}

func TestEphemeralStorageWithExactCountLaunchesSelectedCount(t *testing.T) {
	counts := append([]int{0}, localssd.AllowedLocalSSDCounts("n2d-standard-4", 4)...)
	got := runCreateScenario(t, createConfig{
		mode: v1alpha1.LocalSSDModeEphemeral,
		poolReq: []karpv1.NodeSelectorRequirementWithMinValues{{
			Key: v1alpha1.LabelInstanceLocalSsdCount, Operator: corev1.NodeSelectorOpIn, Values: []string{"2", "4"},
		}},
		podCount: "4", ssdCounts: counts, ephemeralGiB: 800, useProviderMenu: true,
	})
	require.NoError(t, got.appError)
	require.Equal(t, []string{"4"}, got.schedulerCounts)
	require.Equal(t, "4", got.serializedCount)
	require.NoError(t, got.createError)
	require.Equal(t, "4", got.insertedCount)
	require.Equal(t, 4, got.scratchDisks)
}

func TestResolveInstanceTypeFromInstanceUsesFullCatalogForKeylessPool(t *testing.T) {
	ctx := context.Background()
	nodeClass := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nodePool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
		NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
	}}}}
	variants := []*corecloud.InstanceType{
		{Name: "n2d-standard-4", Requirements: scheduling.NewRequirements(scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"))},
		{Name: "n2d-standard-4", Requirements: scheduling.NewRequirements(scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "4"))},
	}
	kc := reproClient{nodeClass: nodeClass, nodePool: nodePool}
	provider := New(kc, reproEvents{}, reproTypes{variants: variants}, nil)
	instance := &instance.Instance{Type: "n2d-standard-4", Labels: map[string]string{
		utils.SanitizeGCELabelValue(utils.LabelNodePoolKey):              "pool",
		utils.SanitizeGCELabelValue(v1alpha1.LabelInstanceLocalSsdCount): "4",
	}}

	menu := instanceTypesForScheduling(nodePool, variants)
	require.Len(t, menu, 1)
	require.Equal(t, "0", menu[0].Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount).Any())
	resolved, err := provider.resolveInstanceTypeFromInstance(ctx, instance)
	require.NoError(t, err)
	require.Equal(t, "4", resolved.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount).Any(), "reconciliation must bypass the count0-only scheduling menu")
}

func TestResolveInstanceTypeFromInstanceIgnoresMissingOwners(t *testing.T) {
	t.Parallel()

	nodeClass := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nodePool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
		NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
	}}}}
	instance := &instance.Instance{Type: "n2d-standard-4", Labels: map[string]string{
		utils.SanitizeGCELabelValue(utils.LabelNodePoolKey): "pool",
	}}

	for _, tc := range []struct {
		name              string
		nodePoolNotFound  bool
		nodeClassNotFound bool
	}{
		{name: "NodePool not found", nodePoolNotFound: true},
		{name: "NodeClass not found", nodeClassNotFound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kc := reproClient{
				nodeClass: nodeClass, nodePool: nodePool,
				nodePoolNotFound: tc.nodePoolNotFound, nodeClassNotFound: tc.nodeClassNotFound,
			}
			provider := New(kc, reproEvents{}, reproTypes{}, nil)
			resolved, err := provider.resolveInstanceTypeFromInstance(context.Background(), instance)
			require.NoError(t, err)
			require.Nil(t, resolved)
		})
	}
}

func TestAmbiguousCountAdoptsExistingInstanceBeforeLaunchGate(t *testing.T) {
	counts := append([]int{0}, localssd.AllowedLocalSSDCounts("n2d-standard-4", 4)...)
	got := runCreateScenario(t, createConfig{
		mode: v1alpha1.LocalSSDModeEphemeral,
		poolReq: []karpv1.NodeSelectorRequirementWithMinValues{{
			Key: v1alpha1.LabelInstanceLocalSsdCount, Operator: corev1.NodeSelectorOpIn, Values: []string{"2", "4"},
		}},
		ssdCounts: counts, cpuMatrix: true, useProviderMenu: true, existingInstance: true,
	})
	require.NoError(t, got.appError)
	require.Equal(t, []string{"2", "4"}, got.schedulerCounts)
	require.NoError(t, got.createError, "an existing response-lost VM must be adopted before rejecting a broad new launch")
	require.Zero(t, got.insertAttempts)
}

type createResult struct {
	schedulerCounts  []string
	serializedCount  string
	insertedCount    string
	scratchDisks     int
	insertAttempts   int
	newNodeClaims    int
	appError         error
	createError      error
	allocatableCPU   map[string]int64
	applicationCPU   int64
	originalCapacity map[string]corev1.ResourceList
	capacity         map[string]corev1.ResourceList
}

type createConfig struct {
	mode             v1alpha1.LocalSSDMode
	poolReq          []karpv1.NodeSelectorRequirementWithMinValues
	podCount         string
	daemonCount      string
	ssdCounts        []int
	ephemeralGiB     int64
	cpuMatrix        bool
	useProviderMenu  bool
	existingInstance bool
}

func runCreateScenario(t *testing.T, config createConfig) createResult { //nolint:gocyclo
	t.Helper()
	ctx := karpopts.ToContext(context.Background(), &karpopts.Options{})
	ctx = gcpoptions.ToContext(ctx, &gcpoptions.Options{VMMemoryOverheadPercent: .07})
	mt := &computepb.MachineType{Name: lo.ToPtr("n2d-standard-4"), GuestCpus: lo.ToPtr[int32](4), MemoryMb: lo.ToPtr[int32](16384)}
	off := corecloud.Offerings{&corecloud.Offering{Available: true, Price: 1, Requirements: scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"), scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand))}}
	nclass := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: v1alpha1.GCENodeClassSpec{LocalSsdMode: config.mode, Disks: []v1alpha1.Disk{{Boot: true, SizeGiB: 100}}, ServiceAccount: "node@test"}, Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{{SourceImage: "projects/test/global/images/cos"}}}}
	nclass.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
	if len(config.ssdCounts) == 0 {
		config.ssdCounts = []int{2, 4}
	}
	variants := make([]*corecloud.InstanceType, 0, len(config.ssdCounts))
	for _, count := range config.ssdCounts {
		it := instancetype.NewInstanceType(ctx, mt, nclass, "us-central1", off, count)
		require.NotNil(t, it)
		variants = append(variants, it)
	}
	result := createResult{
		allocatableCPU:   map[string]int64{},
		originalCapacity: map[string]corev1.ResourceList{},
		capacity:         map[string]corev1.ResourceList{},
	}
	for _, it := range variants {
		count := it.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount).Any()
		result.originalCapacity[count] = it.Capacity.DeepCopy()
		if config.cpuMatrix {
			allocatableCPU := it.Allocatable()[corev1.ResourceCPU]
			result.allocatableCPU[count] = allocatableCPU.MilliValue()
		}
	}
	poolReq := []karpv1.NodeSelectorRequirementWithMinValues{{
		Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{mt.GetName()},
	}}
	poolReq = append(poolReq, config.poolReq...)
	pool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool", UID: types.UID("pool")}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{Requirements: poolReq, NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"}}}}}
	kc := reproClient{nodeClass: nclass}
	typeProvider := reproTypes{variants: variants, machine: mt}
	menu := variants
	if config.useProviderMenu {
		menuProvider := New(kc, reproEvents{}, typeProvider, nil)
		var err error
		menu, err = menuProvider.GetInstanceTypes(ctx, pool)
		require.NoError(t, err)
	}
	its := map[string][]*corecloud.InstanceType{"pool": menu}
	cluster := state.NewCluster(clock.RealClock{}, kc, nil)
	topo, err := corescheduling.NewTopology(ctx, kc, cluster, nil, []*karpv1.NodePool{pool}, its, nil)
	require.NoError(t, err)
	var daemons []*corev1.Pod
	var app *corev1.Pod
	if config.cpuMatrix {
		applicationCPU := itCPUAllocatable(t, variants, "2").DeepCopy()
		applicationCPU.Sub(*resource.NewMilliQuantity(50, resource.DecimalSI))
		result.applicationCPU = applicationCPU.MilliValue()
		require.Positive(t, result.applicationCPU)
		app = reproCPUPod("app", applicationCPU, nil)
		if config.daemonCount != "" {
			daemons = []*corev1.Pod{reproCPUPod("daemon", *resource.NewMilliQuantity(100, resource.DecimalSI), map[string]string{v1alpha1.LabelInstanceLocalSsdCount: config.daemonCount})}
		}
	} else {
		ephemeralGiB := config.ephemeralGiB
		if ephemeralGiB == 0 {
			ephemeralGiB = 500
		}
		app = reproPod("app", ephemeralGiB, nil)
		if config.daemonCount != "" {
			daemons = []*corev1.Pod{reproPod("daemon", 300, map[string]string{v1alpha1.LabelInstanceLocalSsdCount: config.daemonCount})}
		}
	}
	if config.podCount != "" {
		setRequiredInstanceAndCountAffinity(app, mt.GetName(), config.podCount)
	}
	s := corescheduling.NewScheduler(ctx, kc, []*karpv1.NodePool{pool}, cluster, nil, topo, its, daemons, reproEvents{}, clock.RealClock{}, nil, nil)
	results, err := s.Solve(ctx, []*corev1.Pod{app})
	require.NoError(t, err)
	result.appError = results.PodErrors[app]
	result.newNodeClaims = len(results.NewNodeClaims)
	if !config.cpuMatrix {
		require.Empty(t, results.PodErrors)
		require.Len(t, results.NewNodeClaims, 1)
	}
	if len(results.NewNodeClaims) > 0 {
		for _, it := range results.NewNodeClaims[0].InstanceTypeOptions {
			result.schedulerCounts = append(result.schedulerCounts, it.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount).Any())
		}
	}
	var mu sync.Mutex
	var inserted *compute.Instance
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/projects/test/zones/us-central1-a/instances" {
			var request compute.Instance
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decoding Compute insert: %v", err)
				http.Error(w, "invalid insert body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			result.insertAttempts++
			inserted = &request
			mu.Unlock()
			reproWriteJSON(w, &compute.Operation{Name: "op", Status: "DONE"})
			return
		}
		if r.URL.Path == "/projects/test/aggregated/instances" {
			items := map[string]compute.InstancesScopedList{}
			if config.existingInstance {
				items["zones/us-central1-a"] = compute.InstancesScopedList{Instances: []*compute.Instance{{
					Name: "karpenter-claim", Zone: "zones/us-central1-a", MachineType: "zones/us-central1-a/machineTypes/n2d-standard-4",
					Labels: map[string]string{utils.SanitizeGCELabelValue(v1alpha1.LabelInstanceLocalSsdCount): "4"},
				}}}
			}
			reproWriteJSON(w, &compute.InstanceAggregatedList{Items: items})
			return
		}
		if r.URL.Path == "/projects/test/zones/us-central1-a/operations/op" {
			reproWriteJSON(w, &compute.Operation{Name: "op", Status: "DONE"})
			return
		}
		http.Error(w, r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()
	svc, err := compute.NewService(ctx, googleoption.WithEndpoint(srv.URL+"/"), googleoption.WithoutAuthentication())
	require.NoError(t, err)
	ip := instance.NewProvider("cluster", "us-central1", "us-central1", "test", "node@test", "", svc, reproGKE{}, typeProvider, reproTemplate{}, reproVersion{}, unavailableofferings.NewUnavailableOfferings())
	cp := New(kc, reproEvents{}, typeProvider, ip)
	if len(results.NewNodeClaims) > 0 {
		generated := results.NewNodeClaims[0].ToNodeClaim()
		generated.Name = "claim"
		generated.Labels[karpv1.NodePoolLabelKey] = "pool"
		for _, r := range generated.Spec.Requirements {
			if r.Key == v1alpha1.LabelInstanceLocalSsdCount {
				result.serializedCount = strings.Join(r.Values, ",")
			}
		}
		_, result.createError = cp.Create(ctx, generated)
	}
	mu.Lock()
	defer mu.Unlock()
	if inserted != nil {
		result.insertedCount = inserted.Labels[utils.SanitizeGCELabelValue(v1alpha1.LabelInstanceLocalSsdCount)]
		for _, d := range inserted.Disks {
			if d.Type == "SCRATCH" {
				result.scratchDisks++
			}
		}
	}
	for _, it := range variants {
		count := it.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount).Any()
		result.capacity[count] = it.Capacity.DeepCopy()
		if config.cpuMatrix {
			cpu := it.Allocatable()[corev1.ResourceCPU]
			require.Equal(t, result.allocatableCPU[count], cpu.MilliValue(), "scheduling and Create must not mutate allocatable CPU")
		}
	}
	return result
}

func itCPUAllocatable(t *testing.T, variants []*corecloud.InstanceType, count string) resource.Quantity {
	t.Helper()
	for _, it := range variants {
		if it.Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount).Any() == count {
			return it.Allocatable()[corev1.ResourceCPU]
		}
	}
	t.Fatalf("missing local SSD count %q", count)
	return resource.Quantity{}
}

func reproCPUPod(name string, cpu resource.Quantity, selector map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)}, Status: corev1.PodStatus{Phase: corev1.PodPending}, Spec: corev1.PodSpec{NodeSelector: selector, Containers: []corev1.Container{{Name: "c", Image: "x", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: cpu.DeepCopy()}}}}}}
}

func setRequiredInstanceAndCountAffinity(pod *corev1.Pod, machineType, count string) {
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{machineType}},
				{Key: v1alpha1.LabelInstanceLocalSsdCount, Operator: corev1.NodeSelectorOpIn, Values: []string{count}},
			},
		}}},
	}}
}

func reproWriteJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func reproPod(name string, gib int64, selector map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)}, Status: corev1.PodStatus{Phase: corev1.PodPending}, Spec: corev1.PodSpec{NodeSelector: selector, Containers: []corev1.Container{{Name: "c", Image: "x", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: *resource.NewQuantity(gib*1024*1024*1024, resource.BinarySI)}}}}}}
}
