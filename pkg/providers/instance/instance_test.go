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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
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

func TestMergeInstanceTagsPreservesTemplateAndAddsNetworkTags(t *testing.T) {
	base := &compute.Tags{Items: []string{"gke-default", "existing"}, Fingerprint: "fp"}

	merged := mergeInstanceTags(base, []v1alpha1.NetworkTag{"existing", "custom"})

	require.NotNil(t, merged)
	require.Equal(t, []string{"gke-default", "existing", "existing", "custom"}, merged.Items)
	require.Equal(t, "fp", merged.Fingerprint)
	require.Equal(t, []string{"gke-default", "existing"}, base.Items, "base tags should not be mutated")
	require.NotSame(t, base, merged)
}

func TestMergeInstanceTagsHandlesNilTemplate(t *testing.T) {
	merged := mergeInstanceTags(nil, []v1alpha1.NetworkTag{"tag-one", "tag-two"})

	require.NotNil(t, merged)
	require.Equal(t, []string{"tag-one", "tag-two"}, merged.Items)
	require.Empty(t, merged.Fingerprint)
}

func TestMergeInstanceTagsReturnsNilWhenNoTags(t *testing.T) {
	require.Nil(t, mergeInstanceTags(nil, nil))
	require.Nil(t, mergeInstanceTags(&compute.Tags{}, nil))
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
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      corev1.LabelTopologyZone,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"europe-west4-b"},
					},
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
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      corev1.LabelTopologyZone,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"europe-west4-b"},
					},
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
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      corev1.LabelTopologyZone,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"europe-west4-a", "europe-west4-b"},
					},
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

func TestSetupNetworkInterfaces(t *testing.T) {
	t.Parallel()

	makeTemplate := func(rangeName string) *compute.InstanceTemplate {
		return &compute.InstanceTemplate{
			Properties: &compute.InstanceProperties{
				NetworkInterfaces: []*compute.NetworkInterface{
					{
						AliasIpRanges: []*compute.AliasIpRange{
							{SubnetworkRangeName: rangeName, IpCidrRange: "/24"},
						},
					},
				},
			},
		}
	}

	p := &DefaultProvider{}

	t.Run("without SubnetRangeName, SubnetworkRangeName is unchanged", func(t *testing.T) {
		t.Parallel()

		nodeClass := &v1alpha1.GCENodeClass{}
		template := makeTemplate("original-pods-range")

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Len(t, result[0].AliasIpRanges, 1)
		require.Equal(t, "original-pods-range", result[0].AliasIpRanges[0].SubnetworkRangeName)
		require.Equal(t, "original-pods-range", template.Properties.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName, "template must not be mutated")
	})

	t.Run("with SubnetRangeName, SubnetworkRangeName is overridden", func(t *testing.T) {
		t.Parallel()

		rangeName := "pods-secondary"
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				SubnetRangeName: &rangeName,
			},
		}
		template := makeTemplate("original-pods-range")

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Len(t, result[0].AliasIpRanges, 1)
		require.Equal(t, "pods-secondary", result[0].AliasIpRanges[0].SubnetworkRangeName)
		require.Equal(t, "original-pods-range", template.Properties.NetworkInterfaces[0].AliasIpRanges[0].SubnetworkRangeName, "template must not be mutated")
	})

	t.Run("CIDR prefix is set from maxPods in both cases", func(t *testing.T) {
		t.Parallel()

		maxPods := int32(32)
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				KubeletConfiguration: &v1alpha1.KubeletConfiguration{MaxPods: &maxPods},
			},
		}
		template := makeTemplate("some-range")

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Equal(t, "/26", result[0].AliasIpRanges[0].IpCidrRange)
		require.Equal(t, "/24", template.Properties.NetworkInterfaces[0].AliasIpRanges[0].IpCidrRange, "template must not be mutated")

		rangeName := "pods-secondary"
		nodeClass.Spec.SubnetRangeName = &rangeName
		result = p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Equal(t, "/26", result[0].AliasIpRanges[0].IpCidrRange)
	})

	// makeTemplateWithAccessConfig builds on makeTemplate, adding an access config and subnetwork
	// to the primary interface to simulate a public-node pool template.
	makeTemplateWithAccessConfig := func() *compute.InstanceTemplate {
		tmpl := makeTemplate("pods")
		tmpl.Properties.NetworkInterfaces[0].AccessConfigs = []*compute.AccessConfig{
			{Type: "ONE_TO_ONE_NAT", Name: "External NAT"},
		}
		tmpl.Properties.NetworkInterfaces[0].Subnetwork = "regions/us-central1/subnetworks/default"
		return tmpl
	}

	t.Run("without NetworkConfig, AccessConfigs are inherited from template", func(t *testing.T) {
		t.Parallel()

		nodeClass := &v1alpha1.GCENodeClass{}
		template := makeTemplateWithAccessConfig()

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Len(t, result[0].AccessConfigs, 1, "access config should be inherited from template")
	})

	t.Run("EnableExternalIPAccess=false removes AccessConfigs (private node)", func(t *testing.T) {
		t.Parallel()

		disabled := false
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					NetworkInterfaces: []v1alpha1.NetworkInterface{
						{EnableExternalIPAccess: &disabled},
					},
				},
			},
		}
		template := makeTemplateWithAccessConfig()

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Empty(t, result[0].AccessConfigs, "access configs must be removed for private node")
		require.Contains(t, result[0].ForceSendFields, "AccessConfigs", "AccessConfigs must be force-sent as empty so GCP API does not default-insert ONE_TO_ONE_NAT")
		require.Len(t, template.Properties.NetworkInterfaces[0].AccessConfigs, 1, "template must not be mutated")
		require.NotContains(t, template.Properties.NetworkInterfaces[0].ForceSendFields, "AccessConfigs", "template ForceSendFields must not be mutated")
	})

	t.Run("EnableExternalIPAccess=true on template with no AccessConfigs leaves them empty (no-op)", func(t *testing.T) {
		t.Parallel()

		enabled := true
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					NetworkInterfaces: []v1alpha1.NetworkInterface{
						{EnableExternalIPAccess: &enabled},
					},
				},
			},
		}
		// makeTemplate produces a template with no AccessConfigs.
		template := makeTemplate("pods")

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Empty(t, result[0].AccessConfigs, "EnableExternalIPAccess=true does not synthesize an access config; template value is inherited")
	})

	t.Run("EnableExternalIPAccess=true keeps AccessConfigs from template", func(t *testing.T) {
		t.Parallel()

		enabled := true
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					NetworkInterfaces: []v1alpha1.NetworkInterface{
						{EnableExternalIPAccess: &enabled},
					},
				},
			},
		}
		template := makeTemplateWithAccessConfig()

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Len(t, result[0].AccessConfigs, 1, "access configs should be kept when explicitly enabled")
	})

	t.Run("NetworkConfig.Subnetwork overrides template subnetwork", func(t *testing.T) {
		t.Parallel()

		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					NetworkInterfaces: []v1alpha1.NetworkInterface{
						{Subnetwork: "regions/us-central1/subnetworks/private-subnet"},
					},
				},
			},
		}
		template := makeTemplateWithAccessConfig()

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Equal(t, "regions/us-central1/subnetworks/private-subnet", result[0].Subnetwork)
		require.Equal(t, "regions/us-central1/subnetworks/default", template.Properties.NetworkInterfaces[0].Subnetwork, "template must not be mutated")
	})

	t.Run("NetworkConfig with fewer interfaces than template does not override unmatched interfaces", func(t *testing.T) {
		t.Parallel()

		disabled := false
		// Template with two interfaces; NodeClass overrides only the first.
		template := &compute.InstanceTemplate{
			Properties: &compute.InstanceProperties{
				NetworkInterfaces: []*compute.NetworkInterface{
					{
						AccessConfigs: []*compute.AccessConfig{{Type: "ONE_TO_ONE_NAT"}},
						AliasIpRanges: []*compute.AliasIpRange{{SubnetworkRangeName: "pods", IpCidrRange: "/24"}},
					},
					{
						AccessConfigs: []*compute.AccessConfig{{Type: "ONE_TO_ONE_NAT"}},
						AliasIpRanges: []*compute.AliasIpRange{},
					},
				},
			},
		}
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					NetworkInterfaces: []v1alpha1.NetworkInterface{
						{EnableExternalIPAccess: &disabled},
						// second interface not listed — should be unmodified
					},
				},
			},
		}

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 2)
		require.Empty(t, result[0].AccessConfigs, "first interface should have no external IP")
		require.Len(t, result[1].AccessConfigs, 1, "second interface should inherit access config from template")
	})

	t.Run("NetworkConfig present with empty NetworkInterfaces inherits all template settings", func(t *testing.T) {
		t.Parallel()

		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					NetworkInterfaces: []v1alpha1.NetworkInterface{},
				},
			},
		}
		template := makeTemplateWithAccessConfig()

		result := p.setupNetworkInterfaces(template, nodeClass)

		require.Len(t, result, 1)
		require.Len(t, result[0].AccessConfigs, 1, "empty NetworkInterfaces list must not override template")
		require.Equal(t, "regions/us-central1/subnetworks/default", result[0].Subnetwork, "empty NetworkInterfaces list must not override template subnetwork")
	})

	t.Run("EnableExternalIPAccess=false does not duplicate AccessConfigs in ForceSendFields when already present", func(t *testing.T) {
		t.Parallel()

		disabled := false
		nodeClass := &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				NetworkConfig: &v1alpha1.NetworkConfig{
					NetworkInterfaces: []v1alpha1.NetworkInterface{
						{EnableExternalIPAccess: &disabled},
					},
				},
			},
		}
		tmpl := makeTemplateWithAccessConfig()
		tmpl.Properties.NetworkInterfaces[0].ForceSendFields = []string{"AccessConfigs"}

		result := p.setupNetworkInterfaces(tmpl, nodeClass)

		require.Len(t, result, 1)
		require.Empty(t, result[0].AccessConfigs, "access configs must be removed for private node")
		count := 0
		for _, f := range result[0].ForceSendFields {
			if f == "AccessConfigs" {
				count++
			}
		}
		require.Equal(t, 1, count, "AccessConfigs must appear exactly once in ForceSendFields")
	})
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
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      karpv1.CapacityTypeLabelKey,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{karpv1.CapacityTypeSpot, karpv1.CapacityTypeOnDemand},
					},
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
					NodeSelectorRequirement: corev1.NodeSelectorRequirement{
						Key:      karpv1.CapacityTypeLabelKey,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{karpv1.CapacityTypeOnDemand},
					},
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

	instance, err := p.buildInstance(
		spotOrOnDemandNodeClaim(),
		&v1alpha1.GCENodeClass{},
		onDemandOnlyIT,
		template,
		"default-pool", "us-central1-a", "karpenter-test",
		karpv1.CapacityTypeSpot, // externally decided by Create() — must not be recomputed
	)

	require.NoError(t, err)
	require.NotNil(t, instance)
	require.Equal(t, "SPOT", instance.Scheduling.ProvisioningModel,
		"buildInstance must use the passed capacityType, not recompute it from instanceType offerings")
}

func TestSetupInstanceLabels_StampsClusterLocation(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{
		clusterName:     "my-cluster",
		clusterLocation: "us-central1-f",
	}
	inst := &compute.Instance{Labels: make(map[string]string)}
	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{karpv1.NodePoolLabelKey: "my-pool"},
		},
	}
	nodeClass := &v1alpha1.GCENodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "my-nc"},
	}
	it := amd64InstanceType()

	p.setupInstanceLabels(inst, nodeClaim, nodeClass, it)

	locationKey := utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)
	require.Equal(t, "us-central1-f", inst.Labels[locationKey],
		"cluster location must be stamped on the instance label %q", locationKey)
}

func TestSetupInstanceLabels_ClusterNameNotOverwrittenByRequirements(t *testing.T) {
	t.Parallel()

	p := &DefaultProvider{
		clusterName:     "real-cluster",
		clusterLocation: "us-central1-f",
	}
	inst := &compute.Instance{Labels: make(map[string]string)}
	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{karpv1.NodePoolLabelKey: "my-pool"},
		},
	}
	nodeClass := &v1alpha1.GCENodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "my-nc"},
	}
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

	// A NodePool whose requirements carry the cluster-location label key must not
	// be able to overwrite the controller-stamped value.
	p := &DefaultProvider{
		clusterName:     "my-cluster",
		clusterLocation: "us-central1-f",
	}
	inst := &compute.Instance{Labels: make(map[string]string)}
	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{karpv1.NodePoolLabelKey: "my-pool"},
		},
	}
	nodeClass := &v1alpha1.GCENodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "my-nc"},
	}
	it := &cloudprovider.InstanceType{
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			// Simulate a user-supplied NodePool label with the same key as the location label.
			scheduling.NewRequirement(utils.LabelClusterLocationKey, corev1.NodeSelectorOpIn, "attacker-region"),
		),
	}

	p.setupInstanceLabels(inst, nodeClaim, nodeClass, it)

	locationKey := utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)
	require.Equal(t, "us-central1-f", inst.Labels[locationKey],
		"cluster location must not be overwritten by a NodePool label with the same key")
}

func TestBelongsToCluster(t *testing.T) {
	t.Parallel()

	const controllerLocation = "us-central1-f"
	const controllerRegion = "us-central1"
	locationKey := utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey)

	cases := []struct {
		name      string
		labels    map[string]string
		zone      string
		want      bool
	}{
		{
			name:   "label present and matches controller location",
			labels: map[string]string{locationKey: controllerLocation},
			zone:   "us-central1-f",
			want:   true,
		},
		{
			name:   "label present but belongs to a different cluster location",
			labels: map[string]string{locationKey: "us-east1-b"},
			zone:   "us-east1-b",
			want:   false,
		},
		{
			name:   "label absent, zone in same region (legacy node during rolling upgrade)",
			labels: map[string]string{},
			zone:   "us-central1-a",
			want:   true,
		},
		{
			name:   "label absent, zone in different region (same-named cluster, old karpenter)",
			labels: map[string]string{},
			zone:   "us-east1-b",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := &DefaultProvider{clusterLocation: controllerLocation, region: controllerRegion}
			inst := &Instance{InstanceID: "test-instance", Location: tc.zone, Labels: tc.labels}
			require.Equal(t, tc.want, p.belongsToCluster(inst), tc.name)
		})
	}
}

func TestZoneToRegion(t *testing.T) {
	t.Parallel()

	cases := []struct{ zone, want string }{
		{"us-central1-f", "us-central1"},
		{"europe-west1-b", "europe-west1"},
		{"northamerica-northeast1-a", "northamerica-northeast1"},
		{"nozone", "nozone"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, zoneToRegion(tc.zone), tc.zone)
	}
}
