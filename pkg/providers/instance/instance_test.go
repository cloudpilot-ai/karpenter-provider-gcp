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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
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

