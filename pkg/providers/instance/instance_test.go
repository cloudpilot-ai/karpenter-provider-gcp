/*
Copyright 2025 The CloudPilot AI Authors.

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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	containerv1 "google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func TestBuildInstanceTagsAlwaysIncludesClusterTag(t *testing.T) {
	tags := buildInstanceTags("my-cluster", nil)
	require.Contains(t, tags.Items, "gke-my-cluster-node")
}

func TestBuildInstanceTagsAppendsNodeClassTags(t *testing.T) {
	tags := buildInstanceTags("my-cluster", []v1alpha1.NetworkTag{"custom-tag", "another"})
	require.Equal(t, []string{"gke-my-cluster-node", "custom-tag", "another"}, tags.Items)
}

func TestBuildInstanceTagsNoNodeClassTags(t *testing.T) {
	tags := buildInstanceTags("prod-cluster", nil)
	require.Equal(t, []string{"gke-prod-cluster-node"}, tags.Items)
}

func TestIsInsufficientCapacityErrorMatchesCode(t *testing.T) {
	t.Parallel()

	entry := &compute.OperationErrorErrors{Code: "IP_SPACE_EXHAUSTED_WITH_DETAILS"}

	require.True(t, isInsufficientCapacityError(entry))
}

func TestIsInsufficientCapacityErrorIgnoresMessageOnly(t *testing.T) {
	t.Parallel()

	entry := &compute.OperationErrorErrors{Message: "some failure IP_SPACE_EXHAUSTED_WITH_DETAILS for range"}

	require.False(t, isInsufficientCapacityError(entry))
}

func TestIsInsufficientCapacityErrorNonMatching(t *testing.T) {
	t.Parallel()

	entry := &compute.OperationErrorErrors{Code: "UNKNOWN", Message: "other issue"}

	require.False(t, isInsufficientCapacityError(entry))
}

func TestExtractInsertInsufficientCapacityReasonMatchesReason(t *testing.T) {
	t.Parallel()

	reason, code, ok := extractInsertInsufficientCapacityReason(&googleapi.Error{
		Errors: []googleapi.ErrorItem{{Reason: "IP_SPACE_EXHAUSTED_WITH_DETAILS"}},
	})

	require.True(t, ok)
	require.Equal(t, "IP_SPACE_EXHAUSTED_WITH_DETAILS", reason)
	require.Equal(t, "IP_SPACE_EXHAUSTED_WITH_DETAILS", code)
}

func TestExtractInsertInsufficientCapacityReasonRequiresStructuredReason(t *testing.T) {
	t.Parallel()

	reason, code, ok := extractInsertInsufficientCapacityReason(&googleapi.Error{
		Message: "some failure IP_SPACE_EXHAUSTED for range",
	})

	require.False(t, ok)
	require.Empty(t, reason)
	require.Empty(t, code)
}

func TestExtractInsertInsufficientCapacityReasonNonMatching(t *testing.T) {
	t.Parallel()

	_, _, ok := extractInsertInsufficientCapacityReason(&googleapi.Error{Message: "some other issue"})

	require.False(t, ok)
}

func TestInsufficientCapacityBackoffTTLForIPSpace(t *testing.T) {
	t.Parallel()

	ttl := insufficientCapacityBackoffTTL("IP_SPACE_EXHAUSTED")

	require.Equal(t, ipSpaceInsufficientCapacityTTL, ttl)
}

func TestInsufficientCapacityBackoffTTLForOtherReasons(t *testing.T) {
	t.Parallel()

	ttl := insufficientCapacityBackoffTTL("ZONE_RESOURCE_POOL_EXHAUSTED")

	require.Equal(t, pkgcache.UnavailableOfferingsTTL, ttl)
}

// newFakeComputeProvider builds a DefaultProvider whose computeService targets a fake
// HTTP server driven by handler.
func newFakeComputeProvider(t *testing.T, handler http.Handler) *DefaultProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	svc, err := compute.NewService(context.Background(),
		option.WithEndpoint(srv.URL+"/"),
		option.WithoutAuthentication(),
	)
	require.NoError(t, err)
	return &DefaultProvider{
		projectID:      "test-project",
		region:         "us-central1",
		computeService: svc,
		instanceCache:  cache.New(instanceCacheExpiration, instanceCacheExpiration),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestDelete_ReturnsNotFoundWhenInstanceMissing(t *testing.T) {
	t.Parallel()

	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, &googleapi.Error{Code: http.StatusNotFound, Message: "not found"})
	}))

	err := p.Delete(context.Background(), "gce://test-project/us-central1-a/karpenter-node1")

	require.True(t, cloudprovider.IsNodeClaimNotFoundError(err), "expected NodeClaimNotFoundError, got %v", err)
}

func TestDelete_ReturnsNilWhenInstanceIsStopping(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		writeJSON(w, &compute.Instance{Name: "karpenter-node1", Status: InstanceStatusStopping})
	}))

	err := p.Delete(context.Background(), "gce://test-project/us-central1-a/karpenter-node1")

	require.NoError(t, err, "STOPPING should return nil so the reconciler requeues")
	require.False(t, deleteCalled, "a new delete must not be issued while one is already in progress")
}

func TestDelete_ReturnsNodeClaimNotFoundWhenInstanceIsTerminated(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		writeJSON(w, &compute.Instance{Name: "karpenter-node1", Status: InstanceStatusTerminated})
	}))

	err := p.Delete(context.Background(), "gce://test-project/us-central1-a/karpenter-node1")

	require.True(t, cloudprovider.IsNodeClaimNotFoundError(err), "TERMINATED should signal karpenter to finalize the NodeClaim")
	require.False(t, deleteCalled, "no delete should be issued for an already-terminated instance")
}

func TestDelete_IssuesDeleteWhenInstanceIsRunning(t *testing.T) {
	t.Parallel()

	deleteCalled := false
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
			writeJSON(w, &compute.Operation{Name: "op-123", Status: "DONE"})
			return
		}
		writeJSON(w, &compute.Instance{Name: "karpenter-node1", Status: InstanceStatusRunning})
	}))

	err := p.Delete(context.Background(), "gce://test-project/us-central1-a/karpenter-node1")
	require.NoError(t, err)

	require.True(t, deleteCalled, "delete must be issued for a running instance")
}

type fakeGKEProvider struct {
	zones []string
}

func (f *fakeGKEProvider) ResolveClusterZones(context.Context) ([]string, error) {
	return f.zones, nil
}

func (f *fakeGKEProvider) GetClusterConfig(context.Context) (*containerv1.Cluster, error) {
	return &containerv1.Cluster{
		NetworkConfig: &containerv1.NetworkConfig{
			Network:    "projects/test-project/global/networks/default",
			Subnetwork: "projects/test-project/regions/europe-west4/subnetworks/default",
		},
	}, nil
}

func TestSelectZone_OnDemandHonorsTopologyRequirement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	p := &DefaultProvider{
		gkeProvider: &fakeGKEProvider{
			zones: []string{"europe-west4-a", "europe-west4-b", "europe-west4-c"},
		},
		unavailableOfferings: pkgcache.NewUnavailableOfferings(),
	}

	nodeClaim := &karpv1.NodeClaim{
		Spec: karpv1.NodeClaimSpec{
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      corev1.LabelTopologyZone,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"europe-west4-b"},
				},
			},
		},
	}

	instanceType := &cloudprovider.InstanceType{
		Offerings: cloudprovider.Offerings{
			{
				Available: true,
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "europe-west4-b"),
				),
			},
		},
	}

	zone, err := p.selectZone(ctx, nodeClaim, instanceType, karpv1.CapacityTypeOnDemand)
	require.NoError(t, err)
	require.Equal(t, "europe-west4-b", zone)
}

func TestSelectZone_FailsWhenNoZonesMatchRequirement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	p := &DefaultProvider{
		gkeProvider: &fakeGKEProvider{
			zones: []string{"europe-west4-a", "europe-west4-c"},
		},
		unavailableOfferings: pkgcache.NewUnavailableOfferings(),
	}

	nodeClaim := &karpv1.NodeClaim{
		Spec: karpv1.NodeClaimSpec{
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      corev1.LabelTopologyZone,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"europe-west4-b"},
				},
			},
		},
	}

	instanceType := &cloudprovider.InstanceType{
		Offerings: cloudprovider.Offerings{
			{
				Available: true,
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "europe-west4-a"),
				),
			},
		},
	}

	_, err := p.selectZone(ctx, nodeClaim, instanceType, karpv1.CapacityTypeOnDemand)
	require.Error(t, err)
}

func TestAdoptExistingInstance_PopulatesFieldsFromGCEInstance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	p := &DefaultProvider{projectID: "my-project"}

	gceInstance := &compute.Instance{
		Name:              "karpenter-test-node",
		Zone:              "https://www.googleapis.com/compute/v1/projects/my-project/zones/us-west4-a",
		MachineType:       "https://www.googleapis.com/compute/v1/projects/my-project/zones/us-west4-a/machineTypes/n2-standard-8",
		Status:            "RUNNING",
		CreationTimestamp: "2025-10-31T11:09:17.350-07:00",
		Labels:            map[string]string{"karpenter-sh-nodepool": "my-pool"},
		Scheduling: &compute.Scheduling{
			ProvisioningModel: "SPOT",
		},
		Disks: []*compute.AttachedDisk{
			{
				Boot: true,
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: "projects/gke-node-images/global/images/gke-cos",
				},
			},
		},
	}

	result := p.adoptExistingInstance(ctx, gceInstance, karpv1.CapacityTypeSpot)

	require.Equal(t, "karpenter-test-node", result.InstanceID)
	require.Equal(t, "karpenter-test-node", result.Name)
	require.Equal(t, "us-west4-a", result.Location, "zone URL should be trimmed to zone name")
	require.Equal(t, "n2-standard-8", result.Type, "machineType URL should be trimmed to type name")
	require.Equal(t, "my-project", result.ProjectID)
	require.Equal(t, "projects/gke-node-images/global/images/gke-cos", result.ImageID)
	require.Equal(t, karpv1.CapacityTypeSpot, result.CapacityType)
	require.Equal(t, "RUNNING", result.Status, "status must reflect the actual instance state, not hardcoded PROVISIONING")
	require.Equal(t, gceInstance.Labels, result.Labels)
	require.Equal(t, gceInstance.Labels, result.Tags, "Tags must be populated (matches syncInstances behavior)")
	require.False(t, result.CreationTime.IsZero(), "CreationTime must not be zero")

	// Verify the creation time was parsed from the instance timestamp, not time.Now()
	expectedTime, err := time.Parse(time.RFC3339, "2025-10-31T11:09:17.350-07:00")
	require.NoError(t, err)
	require.Equal(t, expectedTime, result.CreationTime)
}

func TestAdoptExistingInstance_UsesActualStatusNotProvisioning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	p := &DefaultProvider{projectID: "my-project"}

	for _, status := range []string{"RUNNING", "STAGING", "PROVISIONING"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			gceInstance := &compute.Instance{
				Name:              "karpenter-test-node",
				Zone:              "us-west4-a",
				MachineType:       "n2-standard-8",
				Status:            status,
				CreationTimestamp: "2025-10-31T11:09:17Z",
			}
			result := p.adoptExistingInstance(ctx, gceInstance, karpv1.CapacityTypeOnDemand)
			require.Equal(t, status, result.Status)
		})
	}
}

func TestSelectZone_SpotChoosesCheapestWithinTopologyRequirement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	p := &DefaultProvider{
		gkeProvider: &fakeGKEProvider{
			zones: []string{"europe-west4-a", "europe-west4-b"},
		},
		unavailableOfferings: pkgcache.NewUnavailableOfferings(),
	}

	nodeClaim := &karpv1.NodeClaim{
		Spec: karpv1.NodeClaimSpec{
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      corev1.LabelTopologyZone,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"europe-west4-a", "europe-west4-b"},
				},
			},
		},
	}

	instanceType := &cloudprovider.InstanceType{
		Offerings: cloudprovider.Offerings{
			{
				Available: true,
				Price:     1.0,
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "europe-west4-a"),
				),
			},
			{
				Available: true,
				Price:     0.5,
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "europe-west4-b"),
				),
			},
		},
	}

	zone, err := p.selectZone(ctx, nodeClaim, instanceType, karpv1.CapacityTypeSpot)
	require.NoError(t, err)
	require.Equal(t, "europe-west4-b", zone)
}

func TestSelectZone_OnDemandSkipsUnavailableZones(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	uo := pkgcache.NewUnavailableOfferings()
	uo.MarkUnavailable(ctx, "ICE", "n2-standard-4", "europe-west4-a", karpv1.CapacityTypeOnDemand)
	uo.MarkUnavailable(ctx, "ICE", "n2-standard-4", "europe-west4-c", karpv1.CapacityTypeOnDemand)

	p := &DefaultProvider{
		gkeProvider: &fakeGKEProvider{
			zones: []string{"europe-west4-a", "europe-west4-b", "europe-west4-c"},
		},
		unavailableOfferings: uo,
	}
	instanceType := &cloudprovider.InstanceType{
		Name: "n2-standard-4",
		Offerings: cloudprovider.Offerings{
			{Available: true, Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "europe-west4-b"),
			)},
		},
	}

	zone, err := p.selectZone(ctx, &karpv1.NodeClaim{}, instanceType, karpv1.CapacityTypeOnDemand)
	require.NoError(t, err)
	require.Equal(t, "europe-west4-b", zone, "should skip the two unavailable zones and return the only available one")
}

func TestSelectZone_OnDemandReturnsICEWhenAllZonesUnavailable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	uo := pkgcache.NewUnavailableOfferings()
	for _, z := range []string{"europe-west4-a", "europe-west4-b", "europe-west4-c"} {
		uo.MarkUnavailable(ctx, "ICE", "n2-standard-4", z, karpv1.CapacityTypeOnDemand)
	}

	p := &DefaultProvider{
		gkeProvider: &fakeGKEProvider{
			zones: []string{"europe-west4-a", "europe-west4-b", "europe-west4-c"},
		},
		unavailableOfferings: uo,
	}
	instanceType := &cloudprovider.InstanceType{Name: "n2-standard-4"}

	_, err := p.selectZone(ctx, &karpv1.NodeClaim{}, instanceType, karpv1.CapacityTypeOnDemand)
	require.Error(t, err)
	require.True(t, cloudprovider.IsInsufficientCapacityError(err),
		"should return InsufficientCapacityError when all zones are exhausted")
}

func TestSelectZone_SpotSkipsUnavailableZones(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	uo := pkgcache.NewUnavailableOfferings()
	uo.MarkUnavailable(ctx, "ICE", "n2-standard-4", "europe-west4-a", karpv1.CapacityTypeSpot)

	p := &DefaultProvider{
		gkeProvider: &fakeGKEProvider{
			zones: []string{"europe-west4-a", "europe-west4-b"},
		},
		unavailableOfferings: uo,
	}
	instanceType := &cloudprovider.InstanceType{
		Name: "n2-standard-4",
		Offerings: cloudprovider.Offerings{
			{Available: true, Price: 1.0, Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "europe-west4-a"),
			)},
			{Available: true, Price: 0.5, Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "europe-west4-b"),
			)},
		},
	}

	zone, err := p.selectZone(ctx, &karpv1.NodeClaim{}, instanceType, karpv1.CapacityTypeSpot)
	require.NoError(t, err)
	require.Equal(t, "europe-west4-b", zone, "should skip the ICE-cached zone even though it has a cheaper offering")
}

func TestSelectZone_SpotReturnsICEWhenAllZonesUnavailable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	uo := pkgcache.NewUnavailableOfferings()
	for _, z := range []string{"europe-west4-a", "europe-west4-b"} {
		uo.MarkUnavailable(ctx, "ICE", "n2-standard-4", z, karpv1.CapacityTypeSpot)
	}

	p := &DefaultProvider{
		gkeProvider: &fakeGKEProvider{
			zones: []string{"europe-west4-a", "europe-west4-b"},
		},
		unavailableOfferings: uo,
	}
	instanceType := &cloudprovider.InstanceType{Name: "n2-standard-4"}

	_, err := p.selectZone(ctx, &karpv1.NodeClaim{}, instanceType, karpv1.CapacityTypeSpot)
	require.Error(t, err)
	require.True(t, cloudprovider.IsInsufficientCapacityError(err),
		"should return InsufficientCapacityError when all zones are exhausted for spot")
}

// amd64InstanceType returns a minimal InstanceType with amd64 architecture requirements.
func amd64InstanceType() *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
		),
	}
}

// nonBootDisk builds a GCENodeClass containing a single non-boot data disk.
func nonBootDisk(category v1alpha1.DiskCategory, iops, throughput *int64) *v1alpha1.GCENodeClass {
	disk := v1alpha1.Disk{
		SizeGiB:               100,
		Category:              category,
		Boot:                  false,
		ProvisionedIOPS:       iops,
		ProvisionedThroughput: throughput,
	}
	return &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{disk},
		},
	}
}

// bootDiskNodeClass builds a GCENodeClass with a single boot disk and a resolved amd64 image.
func bootDiskNodeClass(category v1alpha1.DiskCategory, iops, throughput *int64) *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{
				{
					SizeGiB:               100,
					Category:              category,
					Boot:                  true,
					ProvisionedIOPS:       iops,
					ProvisionedThroughput: throughput,
				},
			},
		},
		Status: v1alpha1.GCENodeClassStatus{
			Images: []v1alpha1.Image{
				{
					SourceImage: "projects/my-project/global/images/my-image",
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelArchStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"amd64"},
						},
					},
				},
			},
		},
	}
}

func TestRenderDiskProperties_NoProvisioningWhenFieldsAreNil(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{projectID: "my-project"}
	nodeClass := bootDiskNodeClass("pd-ssd", nil, nil)

	disks, err := p.renderDiskProperties(amd64InstanceType(), nodeClass, "us-central1-a")
	require.NoError(t, err)

	require.Len(t, disks, 1)
	require.Zero(t, disks[0].InitializeParams.ProvisionedIops)
	require.Zero(t, disks[0].InitializeParams.ProvisionedThroughput)
}

func TestRenderDiskProperties_SetsProvisionedIOPS(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{projectID: "my-project"}
	nodeClass := bootDiskNodeClass("pd-extreme", ptr.To(int64(5000)), nil)

	disks, err := p.renderDiskProperties(amd64InstanceType(), nodeClass, "us-central1-a")
	require.NoError(t, err)

	require.Len(t, disks, 1)
	require.Equal(t, int64(5000), disks[0].InitializeParams.ProvisionedIops)
	require.Zero(t, disks[0].InitializeParams.ProvisionedThroughput)
}

func TestRenderDiskProperties_SetsProvisionedThroughput(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{projectID: "my-project"}
	nodeClass := bootDiskNodeClass("hyperdisk-throughput", nil, ptr.To(int64(500)))

	disks, err := p.renderDiskProperties(amd64InstanceType(), nodeClass, "us-central1-a")
	require.NoError(t, err)

	require.Len(t, disks, 1)
	require.Zero(t, disks[0].InitializeParams.ProvisionedIops)
	require.Equal(t, int64(500), disks[0].InitializeParams.ProvisionedThroughput)
}

func TestRenderDiskProperties_SetsBothIOPSAndThroughput(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{projectID: "my-project"}
	nodeClass := bootDiskNodeClass("hyperdisk-balanced", ptr.To(int64(10000)), ptr.To(int64(400)))

	disks, err := p.renderDiskProperties(amd64InstanceType(), nodeClass, "us-central1-a")
	require.NoError(t, err)

	require.Len(t, disks, 1)
	require.Equal(t, int64(10000), disks[0].InitializeParams.ProvisionedIops)
	require.Equal(t, int64(400), disks[0].InitializeParams.ProvisionedThroughput)
}

func TestRenderDiskProperties_MultipleDisksSetProvisioningIndependently(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{projectID: "my-project"}
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			Disks: []v1alpha1.Disk{
				{
					SizeGiB:         100,
					Category:        "hyperdisk-balanced",
					Boot:            true,
					ProvisionedIOPS: ptr.To(int64(10000)),
				},
				{
					SizeGiB:  200,
					Category: "pd-ssd",
					Boot:     false,
				},
			},
		},
		Status: v1alpha1.GCENodeClassStatus{
			Images: []v1alpha1.Image{
				{
					SourceImage: "projects/my-project/global/images/my-image",
					Requirements: []corev1.NodeSelectorRequirement{
						{
							Key:      corev1.LabelArchStable,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"amd64"},
						},
					},
				},
			},
		},
	}

	disks, err := p.renderDiskProperties(amd64InstanceType(), nodeClass, "us-central1-a")
	require.NoError(t, err)

	require.Len(t, disks, 2)
	require.Equal(t, int64(10000), disks[0].InitializeParams.ProvisionedIops)
	require.Zero(t, disks[0].InitializeParams.ProvisionedThroughput)
	require.Zero(t, disks[1].InitializeParams.ProvisionedIops)
	require.Zero(t, disks[1].InitializeParams.ProvisionedThroughput)
}

func makeCluster(network, subnetwork, podRangeName string, privateNodes bool) *containerv1.Cluster {
	c := &containerv1.Cluster{
		NetworkConfig: &containerv1.NetworkConfig{
			Network:    network,
			Subnetwork: subnetwork,
		},
		IpAllocationPolicy: &containerv1.IPAllocationPolicy{
			ClusterSecondaryRangeName: podRangeName,
		},
	}
	if privateNodes {
		c.PrivateClusterConfig = &containerv1.PrivateClusterConfig{EnablePrivateNodes: true}
	}
	return c
}

func TestSetupNetworkInterfaces(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{}

	t.Run("network and subnetwork from cluster config", func(t *testing.T) {
		t.Parallel()

		cluster := makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false)
		result := p.setupNetworkInterfaces(cluster, &v1alpha1.GCENodeClass{})

		require.Len(t, result, 1)
		require.Equal(t, "projects/p/global/networks/my-vpc", result[0].Network)
		require.Equal(t, "regions/us-central1/subnetworks/my-subnet", result[0].Subnetwork)
	})

	t.Run("public cluster gets ONE_TO_ONE_NAT access config", func(t *testing.T) {
		t.Parallel()

		cluster := makeCluster("net", "subnet", "pods", false)
		result := p.setupNetworkInterfaces(cluster, &v1alpha1.GCENodeClass{})

		require.Len(t, result, 1)
		require.Len(t, result[0].AccessConfigs, 1)
		require.Equal(t, "ONE_TO_ONE_NAT", result[0].AccessConfigs[0].Type)
	})

	t.Run("private cluster (EnablePrivateNodes=true) has no access config", func(t *testing.T) {
		t.Parallel()

		cluster := makeCluster("net", "subnet", "pods", true)
		result := p.setupNetworkInterfaces(cluster, &v1alpha1.GCENodeClass{})

		require.Len(t, result, 1)
		require.Empty(t, result[0].AccessConfigs)
		require.Contains(t, result[0].ForceSendFields, "AccessConfigs")
	})

	t.Run("NodeClass EnablePrivateNodes=true overrides public cluster", func(t *testing.T) {
		t.Parallel()

		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					EnablePrivateNodes: ptr.To(true),
				},
			},
		}
		cluster := makeCluster("net", "subnet", "pods", false)
		result := p.setupNetworkInterfaces(cluster, nodeClass)

		require.Len(t, result, 1)
		require.Empty(t, result[0].AccessConfigs)
		require.Contains(t, result[0].ForceSendFields, "AccessConfigs")
	})

	t.Run("NodeClass EnablePrivateNodes=false overrides private cluster", func(t *testing.T) {
		t.Parallel()

		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					EnablePrivateNodes: ptr.To(false),
				},
			},
		}
		cluster := makeCluster("net", "subnet", "pods", true)
		result := p.setupNetworkInterfaces(cluster, nodeClass)

		require.Len(t, result, 1)
		require.Len(t, result[0].AccessConfigs, 1)
	})

	t.Run("NodeClass Subnetwork overrides cluster subnetwork", func(t *testing.T) {
		t.Parallel()

		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					Subnetwork: "regions/us-central1/subnetworks/override",
				},
			},
		}
		cluster := makeCluster("net", "regions/us-central1/subnetworks/default", "pods", false)
		result := p.setupNetworkInterfaces(cluster, nodeClass)

		require.Len(t, result, 1)
		require.Equal(t, "regions/us-central1/subnetworks/override", result[0].Subnetwork)
	})

	t.Run("pod CIDR range from cluster IpAllocationPolicy", func(t *testing.T) {
		t.Parallel()

		cluster := makeCluster("net", "subnet", "cluster-pods-range", false)
		result := p.setupNetworkInterfaces(cluster, &v1alpha1.GCENodeClass{})

		require.Len(t, result, 1)
		require.Equal(t, "cluster-pods-range", result[0].AliasIpRanges[0].SubnetworkRangeName)
	})

	t.Run("NodeClass SubnetRangeName overrides cluster pod range", func(t *testing.T) {
		t.Parallel()

		name := "custom-pods"
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{SubnetRangeName: &name},
		}
		cluster := makeCluster("net", "subnet", "cluster-pods-range", false)
		result := p.setupNetworkInterfaces(cluster, nodeClass)

		require.Equal(t, "custom-pods", result[0].AliasIpRanges[0].SubnetworkRangeName)
	})

	t.Run("CIDR prefix derived from maxPods", func(t *testing.T) {
		t.Parallel()

		maxPods := int32(32)
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				KubeletConfiguration: &v1alpha1.KubeletConfiguration{MaxPods: &maxPods},
			},
		}
		cluster := makeCluster("net", "subnet", "pods", false)
		result := p.setupNetworkInterfaces(cluster, nodeClass)

		require.Equal(t, "/26", result[0].AliasIpRanges[0].IpCidrRange)
	})
}

func TestSetupNetworkInterfaces_AdditionalInterfaceWithSubnetwork(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{}
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			NetworkConfig: &v1alpha1.NetworkConfig{
				AdditionalNetworkInterfaces: []v1alpha1.AdditionalNetworkInterface{
					{Subnetwork: "regions/us-central1/subnetworks/secondary"},
				},
			},
		},
	}
	cluster := makeCluster("net", "subnet", "pods", false)
	result := p.setupNetworkInterfaces(cluster, nodeClass)

	require.Len(t, result, 2)
	require.Equal(t, "subnet", result[0].Subnetwork)
	// Public cluster: primary interface gets ONE_TO_ONE_NAT.
	require.Len(t, result[0].AccessConfigs, 1)
	require.Equal(t, "ONE_TO_ONE_NAT", result[0].AccessConfigs[0].Type)
	require.Equal(t, "regions/us-central1/subnetworks/secondary", result[1].Subnetwork)
	require.Equal(t, "net", result[1].Network)
	// Public cluster: secondary interface also gets ONE_TO_ONE_NAT.
	require.Len(t, result[1].AccessConfigs, 1)
	require.Equal(t, "ONE_TO_ONE_NAT", result[1].AccessConfigs[0].Type)
}

func TestSetupNetworkInterfaces_AdditionalInterfaceNetworkOverride(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{}
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			NetworkConfig: &v1alpha1.NetworkConfig{
				AdditionalNetworkInterfaces: []v1alpha1.AdditionalNetworkInterface{
					{
						Network:    "projects/p/global/networks/other-vpc",
						Subnetwork: "regions/us-central1/subnetworks/secondary",
					},
				},
			},
		},
	}
	cluster := makeCluster("projects/p/global/networks/my-vpc", "subnet", "pods", false)
	result := p.setupNetworkInterfaces(cluster, nodeClass)

	require.Len(t, result, 2)
	require.Equal(t, "projects/p/global/networks/other-vpc", result[1].Network)
}

func TestSetupNetworkInterfaces_NodeClassEnablePrivateNodesWithAdditionalInterfaces(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{}
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			NetworkConfig: &v1alpha1.NetworkConfig{
				EnablePrivateNodes: ptr.To(true),
				AdditionalNetworkInterfaces: []v1alpha1.AdditionalNetworkInterface{
					{Subnetwork: "regions/us-central1/subnetworks/secondary"},
				},
			},
		},
	}
	// Public cluster: NodeClass override drives private behavior on both interfaces.
	cluster := makeCluster("net", "subnet", "pods", false)
	result := p.setupNetworkInterfaces(cluster, nodeClass)

	require.Len(t, result, 2)
	require.Empty(t, result[0].AccessConfigs)
	require.Contains(t, result[0].ForceSendFields, "AccessConfigs")
	require.Empty(t, result[1].AccessConfigs)
	require.Contains(t, result[1].ForceSendFields, "AccessConfigs")
}

func TestSetupNetworkInterfaces_AdditionalInterfacePrivateCluster(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{}
	nodeClass := &v1alpha1.GCENodeClass{
		Spec: v1alpha1.GCENodeClassSpec{
			NetworkConfig: &v1alpha1.NetworkConfig{
				AdditionalNetworkInterfaces: []v1alpha1.AdditionalNetworkInterface{
					{Subnetwork: "regions/us-central1/subnetworks/secondary"},
				},
			},
		},
	}
	cluster := makeCluster("net", "subnet", "pods", true)
	result := p.setupNetworkInterfaces(cluster, nodeClass)

	require.Len(t, result, 2)
	require.Empty(t, result[1].AccessConfigs)
	require.Contains(t, result[1].ForceSendFields, "AccessConfigs")
}

// TestSetupScheduling_DoesNotMutateTemplate guards against setupScheduling mutating the shared
// template. The same template pointer is reused across zone and instance-type retries inside
// Create(), so any in-place write to template.Properties.Scheduling would persist across
// iterations and corrupt subsequent instances.
func TestSetupScheduling_DoesNotMutateTemplate(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{}
	template := &compute.InstanceTemplate{
		Properties: &compute.InstanceProperties{
			Scheduling: &compute.Scheduling{OnHostMaintenance: "MIGRATE"},
		},
	}

	sched := p.setupScheduling(template, karpv1.CapacityTypeSpot)

	require.Equal(t, instanceTerminationActionDelete, sched.InstanceTerminationAction,
		"returned Scheduling must have InstanceTerminationAction set for spot")
	require.Empty(t, template.Properties.Scheduling.InstanceTerminationAction,
		"setupScheduling must not write to the original template Scheduling struct")
	require.Equal(t, "MIGRATE", template.Properties.Scheduling.OnHostMaintenance,
		"original template fields must be unchanged")
}

func spotOrOnDemandNodeClaim() *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		Spec: karpv1.NodeClaimSpec{
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      karpv1.CapacityTypeLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{karpv1.CapacityTypeSpot, karpv1.CapacityTypeOnDemand},
				},
			},
		},
	}
}

func onDemandNodeClaim() *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		Spec: karpv1.NodeClaimSpec{
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      karpv1.CapacityTypeLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{karpv1.CapacityTypeOnDemand},
				},
			},
		},
	}
}

func spotOffering() *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available: true,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot),
		),
	}
}

func onDemandOffering() *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available: true,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		),
	}
}

func TestGetCapacityType(t *testing.T) {
	t.Parallel()

	onDemandOnly := &cloudprovider.InstanceType{
		Offerings: cloudprovider.Offerings{onDemandOffering()},
	}
	withSpot := &cloudprovider.InstanceType{
		Offerings: cloudprovider.Offerings{spotOffering(), onDemandOffering()},
	}
	exhaustedSpot := &cloudprovider.InstanceType{
		Offerings: cloudprovider.Offerings{
			{Available: false, Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot),
			)},
			onDemandOffering(),
		},
	}

	tests := []struct {
		name          string
		nodeClaim     *karpv1.NodeClaim
		instanceTypes []*cloudprovider.InstanceType
		want          string
	}{
		{
			// Spot is not the first type — confirms the loop scans all candidates.
			name:          "returns spot when a later instance type has an available spot offering",
			nodeClaim:     spotOrOnDemandNodeClaim(),
			instanceTypes: []*cloudprovider.InstanceType{onDemandOnly, withSpot},
			want:          karpv1.CapacityTypeSpot,
		},
		{
			// All types have only exhausted or absent spot — confirms no false positive.
			// Also validates that exhausted spot offerings (Available: false) are skipped.
			name:          "falls back to on-demand when no available spot offerings exist",
			nodeClaim:     spotOrOnDemandNodeClaim(),
			instanceTypes: []*cloudprovider.InstanceType{exhaustedSpot, onDemandOnly},
			want:          karpv1.CapacityTypeOnDemand,
		},
		{
			// nodeClaim forbids spot; on-demand offering is present and must be selected.
			name:          "respects on-demand-only node claim even when spot offerings exist",
			nodeClaim:     onDemandNodeClaim(),
			instanceTypes: []*cloudprovider.InstanceType{withSpot, onDemandOnly},
			want:          karpv1.CapacityTypeOnDemand,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &DefaultProvider{}
			require.Equal(t, tt.want, p.getCapacityType(tt.nodeClaim, tt.instanceTypes))
		})
	}
}

func TestBuildInstance_UsesExternalCapacityTypeNotRecomputed(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{}

	// on-demand-only: no spot offering — pre-fix buildInstance would recompute "on-demand" from this.
	onDemandOnlyIT := &cloudprovider.InstanceType{
		Name: "n2-standard-4",
		Offerings: cloudprovider.Offerings{
			{Available: true, Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			)},
		},
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
		),
		Overhead: &cloudprovider.InstanceTypeOverhead{KubeReserved: corev1.ResourceList{}},
	}

	template := &compute.InstanceTemplate{
		Properties: &compute.InstanceProperties{
			Scheduling:        &compute.Scheduling{},
			NetworkInterfaces: []*compute.NetworkInterface{},
			Metadata: &compute.Metadata{
				Items: []*compute.MetadataItems{
					{Key: "kube-labels", Value: ptr.To("max-pods-per-node=110,max-pods=110")},
					{Key: "kube-env", Value: ptr.To("KUBELET_ARGS: --max-pods=110\narch=amd64\n")},
					{Key: "kubelet-config", Value: ptr.To("nodeStatusUpdateFrequency: 10s\n")},
				},
			},
		},
	}

	cluster := makeCluster("projects/p/global/networks/my-vpc", "regions/us-central1/subnetworks/my-subnet", "pods", false)
	instance, err := p.buildInstance(
		spotOrOnDemandNodeClaim(),
		&v1alpha1.GCENodeClass{},
		onDemandOnlyIT,
		template,
		cluster,
		"default-pool", "us-central1-a", "karpenter-test",
		karpv1.CapacityTypeSpot, // externally decided by Create() — must not be recomputed
	)
	require.NoError(t, err)

	require.NotNil(t, instance)
	require.Equal(t, "SPOT", instance.Scheduling.ProvisioningModel,
		"buildInstance must use the passed capacityType, not recompute it from instanceType offerings")
}

// instanceLabelsFixture returns the common NodeClaim and GCENodeClass objects used
// by setupInstanceLabels tests. Only the provider varies between tests.
func instanceLabelsFixture() (*compute.Instance, *karpv1.NodeClaim, *v1alpha1.GCENodeClass) {
	return &compute.Instance{Labels: make(map[string]string)},
		&karpv1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{karpv1.NodePoolLabelKey: "my-pool"},
			},
		},
		&v1alpha1.GCENodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "my-nc"},
		}
}

func TestSetupInstanceLabels_StampsClusterLocation(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{clusterName: "my-cluster", clusterLocation: "us-central1-f"}
	inst, nodeClaim, nodeClass := instanceLabelsFixture()

	p.setupInstanceLabels(inst, nodeClaim, nodeClass, amd64InstanceType())

	locationKey := utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)
	require.Equal(t, "us-central1-f", inst.Labels[locationKey],
		"cluster location must be stamped on the instance label %q", locationKey)
}

func TestSetupInstanceLabels_ClusterNameNotOverwrittenByRequirements(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{clusterName: "real-cluster", clusterLocation: "us-central1-f"}
	inst, nodeClaim, nodeClass := instanceLabelsFixture()
	it := &cloudprovider.InstanceType{
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			scheduling.NewRequirement(utils.LabelClusterNameKey, corev1.NodeSelectorOpIn, "attacker-cluster"),
		),
	}

	p.setupInstanceLabels(inst, nodeClaim, nodeClass, it)

	nameKey := utils.SanitizeGCELabelValue(utils.LabelClusterNameKey)
	require.Equal(t, "real-cluster", inst.Labels[nameKey],
		"cluster name must not be overwritten by a NodePool label with the same key")
}

func TestSetupInstanceLabels_ClusterLocationNotOverwrittenByRequirements(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{clusterName: "my-cluster", clusterLocation: "us-central1-f"}
	inst, nodeClaim, nodeClass := instanceLabelsFixture()
	it := &cloudprovider.InstanceType{
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			scheduling.NewRequirement(utils.LabelClusterLocationKey, corev1.NodeSelectorOpIn, "attacker-region"),
		),
	}

	p.setupInstanceLabels(inst, nodeClaim, nodeClass, it)

	locationKey := utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)
	require.Equal(t, "us-central1-f", inst.Labels[locationKey],
		"cluster location must not be overwritten by a NodePool label with the same key")
}

func TestIsTransientError_TrueFor5xx(t *testing.T) {
	t.Parallel()

	require.True(t, isTransientError(&googleapi.Error{Code: 503}))
	require.True(t, isTransientError(&googleapi.Error{Code: 500}))
}

func TestIsTransientError_FalseForNon5xx(t *testing.T) {
	t.Parallel()

	require.False(t, isTransientError(&googleapi.Error{Code: 404}))
	require.False(t, isTransientError(&googleapi.Error{Code: 429}))
}

func TestIsTransientError_FalseForNonAPIError(t *testing.T) {
	t.Parallel()

	require.False(t, isTransientError(fmt.Errorf("plain error")))
}

func TestWaitOperationDone_RetriesOnTransient503(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(w, &googleapi.Error{Code: http.StatusServiceUnavailable, Message: "Visibility check was unavailable"})
			return
		}
		writeJSON(w, &compute.Operation{Name: "op-123", Status: "DONE"})
	}))

	err := p.waitOperationDone(context.Background(), "n1-standard-1", "us-central1-a", "on-demand", "op-123")
	require.NoError(t, err)

	require.Equal(t, int32(3), callCount.Load())
}

func TestWaitOperationDone_FailsOnPersistent503(t *testing.T) {
	t.Parallel()

	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, &googleapi.Error{Code: http.StatusServiceUnavailable, Message: "Visibility check was unavailable"})
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := p.waitOperationDone(ctx, "n1-standard-1", "us-central1-a", "on-demand", "op-123")

	require.Error(t, err)
}

func TestBelongsToCluster(t *testing.T) {
	t.Parallel()

	const controllerLocation = "us-central1-f"
	locationKey := utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)

	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "label present and matches controller location",
			labels: map[string]string{locationKey: controllerLocation},
			want:   true,
		},
		{
			name:   "label present but different location",
			labels: map[string]string{locationKey: "us-east1-b"},
			want:   false,
		},
		{
			name:   "label absent (pre-location-label instance) — included in cache for backward compatibility",
			labels: map[string]string{},
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := &DefaultProvider{clusterLocation: controllerLocation}
			inst := &Instance{InstanceID: "test-instance", Labels: tc.labels}
			require.Equal(t, tc.want, p.belongsToCluster(inst), tc.name)
		})
	}
}
