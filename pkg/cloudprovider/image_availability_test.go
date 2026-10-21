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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	googleoption "google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	gcpoptions "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
)

const (
	amd64COSImage  = "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre"
	arm64COSImage  = "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre"
	nvidiaCOSImage = "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-nvda"
)

func imageAwareInstanceType(name, arch, gpuCount string) *karpcloudprovider.InstanceType {
	requirements := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, arch),
		scheduling.NewRequirement(v1alpha1.LabelInstanceLocalSsdCount, corev1.NodeSelectorOpIn, "0"),
	)
	if gpuCount == "" {
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist))
	} else {
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpIn, gpuCount))
	}
	return &karpcloudprovider.InstanceType{Name: name, Requirements: requirements}
}

func launchableInstanceType(name, arch, gpuCount string) *karpcloudprovider.InstanceType {
	instanceType := imageAwareInstanceType(name, arch, gpuCount)
	instanceType.Offerings = karpcloudprovider.Offerings{&karpcloudprovider.Offering{
		Available: true,
		Price:     1,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		),
	}}
	instanceType.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("2"),
		corev1.ResourceMemory:           resource.MustParse("8Gi"),
		corev1.ResourcePods:             resource.MustParse("110"),
		corev1.ResourceEphemeralStorage: resource.MustParse("50Gi"),
	}
	instanceType.Overhead = &karpcloudprovider.InstanceTypeOverhead{}
	return instanceType
}

func imagesReadyNodeClass(images ...v1alpha1.Image) *v1alpha1.GCENodeClass {
	nodeClass := &v1alpha1.GCENodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Status:     v1alpha1.GCENodeClassStatus{Images: images},
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
	return nodeClass
}

// TestGetInstanceTypesRequiresUsableImages protects scheduler eligibility: only instance types that
// at least one resolved image can back are offered, and nothing is offered before images are ready.
func TestGetInstanceTypesRequiresUsableImages(t *testing.T) {
	t.Parallel()

	catalog := []*karpcloudprovider.InstanceType{
		imageAwareInstanceType("n2-standard-4", "amd64", ""),
		imageAwareInstanceType("t2a-standard-4", "arm64", ""),
		imageAwareInstanceType("g2-standard-4", "amd64", "1"),
	}
	pool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
		NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
	}}}}
	amd64Image := imageWithRequirements(amd64COSImage, cpuImageRequirements("amd64")...)
	arm64Image := imageWithRequirements(arm64COSImage, cpuImageRequirements("arm64")...)
	nvidiaImage := imageWithRequirements(nvidiaCOSImage, gpuImageRequirements()...)

	names := func(nodeClass *v1alpha1.GCENodeClass) []string {
		t.Helper()
		provider := New(reproClient{nodeClass: nodeClass, nodePool: pool}, reproEvents{}, reproTypes{variants: catalog}, nil, nil)
		got, err := provider.GetInstanceTypes(karpopts.ToContext(context.Background(), &karpopts.Options{}), pool)
		require.NoError(t, err)
		return lo.Map(got, func(it *karpcloudprovider.InstanceType, _ int) string { return it.Name })
	}

	for _, tc := range []struct {
		name      string
		nodeClass *v1alpha1.GCENodeClass
		want      []string
	}{
		{
			name:      "amd64 CPU image schedules only amd64 CPU types",
			nodeClass: imagesReadyNodeClass(amd64Image),
			want:      []string{"n2-standard-4"},
		},
		{
			name:      "each resolved variant schedules its own types",
			nodeClass: imagesReadyNodeClass(amd64Image, arm64Image, nvidiaImage),
			want:      []string{"n2-standard-4", "t2a-standard-4", "g2-standard-4"},
		},
		{
			name: "images that are not ready expose no candidates",
			nodeClass: func() *v1alpha1.GCENodeClass {
				nodeClass := imagesReadyNodeClass(amd64Image)
				nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, "ImageUnavailable", "publication in progress")
				return nodeClass
			}(),
			want: []string{},
		},
		{
			name:      "unknown image readiness exposes no candidates",
			nodeClass: &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{amd64Image}}},
			want:      []string{},
		},
		{
			name:      "ready without images exposes no candidates",
			nodeClass: imagesReadyNodeClass(),
			want:      []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, names(tc.nodeClass))
		})
	}
	require.Len(t, catalog, 3, "the cached catalog must not be mutated")
}

func TestGetInstanceTypesKeepsLocalSSDSchedulingFilter(t *testing.T) {
	t.Parallel()

	// n2d-standard-4 supports configurable local SSDs, so a NodePool that does not opt in must not
	// see its positive-count variants even though the image could boot them.
	catalog := []*karpcloudprovider.InstanceType{
		variantInstanceType("n2d-standard-4", "0"),
		variantInstanceType("n2d-standard-4", "4"),
	}
	pool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
		NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
	}}}}
	nodeClass := imagesReadyNodeClass(imageWithRequirements(amd64COSImage, cpuImageRequirements("amd64")...))
	provider := New(reproClient{nodeClass: nodeClass, nodePool: pool}, reproEvents{}, reproTypes{variants: catalog}, nil, nil)

	got, err := provider.GetInstanceTypes(karpopts.ToContext(context.Background(), &karpopts.Options{}), pool)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "0", got[0].Requirements.Get(v1alpha1.LabelInstanceLocalSsdCount).Any())
}

// TestCreateWithPartialImages protects the adoption boundary: launch candidates are filtered by the
// images that are actually published, but an earlier VM is still adopted before that filter runs.
func TestCreateWithPartialImages(t *testing.T) {
	t.Parallel()

	amd64Type := launchableInstanceType("n2-standard-2", "amd64", "")
	arm64Type := launchableInstanceType("t2a-standard-2", "arm64", "")
	nodeClass := imagesReadyNodeClass(imageWithRequirements(amd64COSImage, cpuImageRequirements("amd64")...))
	nodeClass.Spec = v1alpha1.GCENodeClassSpec{
		Disks:          []v1alpha1.Disk{{Boot: true, SizeGiB: 100}},
		ServiceAccount: "node@test",
	}
	claim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Labels: map[string]string{karpv1.NodePoolLabelKey: "pool"}},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{{
				Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"arm64"},
			}},
			Resources: karpv1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}},
		},
	}

	type outcome struct {
		nodeClaim  *karpv1.NodeClaim
		err        error
		insertions int
	}
	run := func(t *testing.T, existingVM bool) outcome {
		t.Helper()
		result := outcome{}
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/projects/test/aggregated/instances":
				items := map[string]compute.InstancesScopedList{}
				if existingVM {
					items["zones/us-central1-a"] = compute.InstancesScopedList{Instances: []*compute.Instance{{
						Name:              "karpenter-claim",
						Zone:              "zones/us-central1-a",
						MachineType:       "zones/us-central1-a/machineTypes/t2a-standard-2",
						CreationTimestamp: "2025-04-01T00:00:00Z",
						Status:            "RUNNING",
					}}}
				}
				_ = json.NewEncoder(w).Encode(&compute.InstanceAggregatedList{Items: items})
			case strings.HasSuffix(r.URL.Path, "/instances"):
				mu.Lock()
				result.insertions++
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(&compute.Operation{Name: "op", Status: "DONE"})
			default:
				http.Error(w, r.Method+" "+r.URL.Path, http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		service, err := compute.NewService(context.Background(), googleoption.WithEndpoint(srv.URL+"/"), googleoption.WithoutAuthentication())
		require.NoError(t, err)

		unavailable := unavailableofferings.NewUnavailableOfferings()
		instanceProvider := instance.NewProvider("cluster", "us-central1", "us-central1", "test", "node@test", "", service,
			reproGKE{}, nil, reproTypes{variants: []*karpcloudprovider.InstanceType{amd64Type, arm64Type}}, reproTemplate{}, reproVersion{}, unavailable)
		provider := New(reproClient{nodeClass: nodeClass}, reproEvents{}, reproTypes{variants: []*karpcloudprovider.InstanceType{amd64Type, arm64Type}}, instanceProvider, nil)

		ctx := karpopts.ToContext(context.Background(), &karpopts.Options{})
		ctx = gcpoptions.ToContext(ctx, &gcpoptions.Options{VMMemoryOverheadPercent: 0.07})
		result.nodeClaim, result.err = provider.Create(ctx, claim.DeepCopy())
		require.False(t, unavailable.IsUnavailable(arm64Type.Name, "us-central1-a", karpv1.CapacityTypeOnDemand),
			"an image availability problem must not mark the zonal offering unavailable")
		return result
	}

	t.Run("adopts the earlier arm64 VM before image filtering", func(t *testing.T) {
		t.Parallel()
		got := run(t, true)
		require.NoError(t, got.err)
		require.Equal(t, "gce://test/us-central1-a/karpenter-claim", got.nodeClaim.Status.ProviderID)
		require.Zero(t, got.insertions)
	})

	t.Run("rejects a new arm64 launch with no compatible image", func(t *testing.T) {
		t.Parallel()
		got := run(t, false)
		require.Nil(t, got.nodeClaim)
		require.ErrorContains(t, got.err, "no instance type is compatible with any image")
		require.True(t, karpcloudprovider.IsInsufficientCapacityError(got.err),
			"core must recognize an image-selection failure as capacity, not as a NodeClass configuration error: %v", got.err)
		require.Zero(t, got.insertions, "no VM work may happen without a boot image")
	})
}
