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

	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	googleoption "google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
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

// Image availability gates new capacity, not running-node catalog membership or offering identity.
func TestGetInstanceTypesRequiresUsableImages(t *testing.T) {
	t.Parallel()

	catalog := []*karpcloudprovider.InstanceType{
		launchableInstanceType("n2-standard-4", "amd64", ""),
		launchableInstanceType("t2a-standard-4", "arm64", ""),
		launchableInstanceType("g2-standard-4", "amd64", "1"),
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
		require.Len(t, got, len(catalog), "running types must remain visible to core drift checks")
		for idx, it := range got {
			require.True(t, it.Offerings.HasCompatible(catalog[idx].Offerings[0].Requirements),
				"core drift checks require offering identity even when capacity is unavailable")
		}
		available := lo.Filter(got, func(it *karpcloudprovider.InstanceType, _ int) bool { return len(it.Offerings.Available()) > 0 })
		return lo.Map(available, func(it *karpcloudprovider.InstanceType, _ int) string { return it.Name })
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
			name: "images that are not ready expose no available offerings",
			nodeClass: func() *v1alpha1.GCENodeClass {
				nodeClass := imagesReadyNodeClass(amd64Image)
				nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, "ImageUnavailable", "publication in progress")
				return nodeClass
			}(),
			want: []string{},
		},
		{
			name:      "unknown image readiness exposes no available offerings",
			nodeClass: &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{amd64Image}}},
			want:      []string{},
		},
		{
			name:      "ready without images exposes no available offerings",
			nodeClass: imagesReadyNodeClass(),
			want:      []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, names(tc.nodeClass))
		})
	}
	t.Cleanup(func() {
		for _, it := range catalog {
			require.Len(t, it.Offerings.Available(), 1, "the shared catalog must not be mutated")
		}
	})
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

type adoptOnlyInstanceProvider struct {
	instance.Provider
	vm *instance.Instance
}

func (p adoptOnlyInstanceProvider) Adopt(context.Context, *karpv1.NodeClaim) (*instance.Instance, error) {
	return p.vm, nil
}

type overlayClient struct {
	reproClient
	overlay *karpv1alpha1.NodeOverlay
}

func (c overlayClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch list := list.(type) {
	case *karpv1.NodePoolList:
		list.Items = []karpv1.NodePool{*c.nodePool.DeepCopy()}
	case *karpv1alpha1.NodeOverlayList:
		list.Items = []karpv1alpha1.NodeOverlay{*c.overlay.DeepCopy()}
	}
	return nil
}

type overlayStatusWriter struct{ client.SubResourceWriter }

func (overlayStatusWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return nil
}

func (overlayClient) Status() client.SubResourceWriter { return overlayStatusWriter{} }

func TestAdoptionWithUnreadyImagesPreservesNodeOverlay(t *testing.T) {
	t.Parallel()
	for _, readiness := range []string{"false", "unknown"} {
		t.Run(readiness, func(t *testing.T) {
			t.Parallel()
			class := imagesReadyNodeClass()
			if readiness == "false" {
				class.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, "ImageLookupFailed", "lookup failed")
			} else {
				class.StatusConditions().SetUnknown(v1alpha1.ConditionTypeImagesReady)
			}
			pool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
				NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "default"},
			}}}}
			overlay := &karpv1alpha1.NodeOverlay{ObjectMeta: metav1.ObjectMeta{Name: "hugepages"}, Spec: karpv1alpha1.NodeOverlaySpec{
				Capacity: corev1.ResourceList{corev1.ResourceName("hugepages-2Mi"): resource.MustParse("1Gi")},
			}}
			overlay.StatusConditions().SetTrue(karpv1alpha1.ConditionTypeValidationSucceeded)
			kubeClient := overlayClient{reproClient: reproClient{nodeClass: class, nodePool: pool}, overlay: overlay}
			store := nodeoverlay.NewInstanceTypeStore()
			it := launchableInstanceType("t2a-standard-2", "arm64", "")
			it.Offerings[0].Available = false
			provider := New(kubeClient, reproEvents{}, reproTypes{variants: []*karpcloudprovider.InstanceType{it}}, adoptOnlyInstanceProvider{vm: &instance.Instance{
				Name: "karpenter-claim", Type: it.Name, ProjectID: "test", Location: "us-central1-a",
			}}, store)
			ctx := karpopts.ToContext(context.Background(), &karpopts.Options{FeatureGates: karpopts.FeatureGates{NodeOverlay: true}})
			ctx = gcpoptions.ToContext(ctx, &gcpoptions.Options{VMMemoryOverheadPercent: 0.07})
			clk := clock.RealClock{}
			controller := nodeoverlay.NewController(clk, kubeClient, provider, store, state.NewCluster(clk, kubeClient, provider))
			_, err := controller.Reconcile(ctx, reconcile.Request{})
			require.NoError(t, err)
			got, err := provider.Create(ctx, &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Labels: map[string]string{karpv1.NodePoolLabelKey: pool.Name}}, Spec: karpv1.NodeClaimSpec{
				NodeClassRef: pool.Spec.Template.Spec.NodeClassRef,
			}})
			require.NoError(t, err)
			require.Equal(t, resource.MustParse("1Gi"), got.Status.Capacity["hugepages-2Mi"])
			require.Equal(t, resource.MustParse("1Gi"), got.Status.Allocatable["hugepages-2Mi"])
		})
	}
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
	run := func(t *testing.T, existingVM, lookupFails bool, class *v1alpha1.GCENodeClass) outcome {
		t.Helper()
		result := outcome{}
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/projects/test/aggregated/instances":
				if lookupFails {
					http.Error(w, "lookup forbidden", http.StatusForbidden)
					return
				}
				items := map[string]compute.InstancesScopedList{}
				if existingVM {
					items["zones/us-central1-a"] = compute.InstancesScopedList{Instances: []*compute.Instance{{
						Name:              "karpenter-claim",
						Zone:              "zones/us-central1-a",
						MachineType:       "zones/us-central1-a/machineTypes/t2a-standard-2",
						CreationTimestamp: "2025-04-01T00:00:00Z",
						Status:            "RUNNING",
						Scheduling:        &compute.Scheduling{ProvisioningModel: "SPOT"},
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

		// A previous VM remains adoptable even when its offering cannot launch now.
		catalog := []*karpcloudprovider.InstanceType{amd64Type.DeepCopy(), arm64Type.DeepCopy()}
		if !class.StatusConditions().IsTrue(v1alpha1.ConditionTypeImagesReady) {
			catalog[1].Offerings[0].Available = false
		}
		unavailable := unavailableofferings.NewUnavailableOfferings()
		instanceProvider := instance.NewProvider("cluster", "us-central1", "us-central1", "test", "node@test", "", service,
			reproGKE{}, nil, reproTypes{variants: catalog}, reproTemplate{}, reproVersion{}, unavailable)
		provider := New(reproClient{nodeClass: class}, reproEvents{}, reproTypes{variants: catalog}, instanceProvider, nil)

		ctx := karpopts.ToContext(context.Background(), &karpopts.Options{})
		ctx = gcpoptions.ToContext(ctx, &gcpoptions.Options{VMMemoryOverheadPercent: 0.07})
		result.nodeClaim, result.err = provider.Create(ctx, claim.DeepCopy())
		require.False(t, unavailable.IsUnavailable(arm64Type.Name, "us-central1-a", karpv1.CapacityTypeOnDemand),
			"an image availability problem must not mark the zonal offering unavailable")
		return result
	}

	t.Run("adopts the earlier arm64 VM before image filtering", func(t *testing.T) {
		t.Parallel()
		got := run(t, true, false, nodeClass)
		require.NoError(t, got.err)
		require.Equal(t, "gce://test/us-central1-a/karpenter-claim", got.nodeClaim.Status.ProviderID)
		require.Zero(t, got.insertions)
	})

	t.Run("rejects a new arm64 launch with no compatible image", func(t *testing.T) {
		t.Parallel()
		got := run(t, false, false, nodeClass)
		require.Nil(t, got.nodeClaim)
		require.ErrorContains(t, got.err, "no instance type is compatible with any image")
		require.True(t, karpcloudprovider.IsInsufficientCapacityError(got.err),
			"core must recognize an image-selection failure as capacity, not as a NodeClass configuration error: %v", got.err)
		require.Zero(t, got.insertions, "no VM work may happen without a boot image")
	})

	t.Run("unrelated readiness rejection remains blocked", func(t *testing.T) {
		t.Parallel()
		class := nodeClass.DeepCopy()
		class.StatusConditions().SetFalse(status.ConditionReady, "OtherFailure", "not ready")
		got := run(t, true, false, class)
		require.Nil(t, got.nodeClaim)
		require.True(t, karpcloudprovider.IsNodeClassNotReadyError(got.err))
		require.Zero(t, got.insertions)
	})

	for _, readiness := range []string{"false", "unknown"} {
		t.Run(readiness+" image readiness", func(t *testing.T) {
			t.Parallel()
			class := nodeClass.DeepCopy()
			if readiness == "false" {
				class.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, "ImageLookupFailed", "lookup failed")
			} else {
				class.StatusConditions().SetUnknown(v1alpha1.ConditionTypeImagesReady)
			}
			adopted := run(t, true, false, class)
			require.NoError(t, adopted.err)
			require.Equal(t, "gce://test/us-central1-a/karpenter-claim", adopted.nodeClaim.Status.ProviderID)
			require.Equal(t, karpv1.CapacityTypeSpot, adopted.nodeClaim.Labels[karpv1.CapacityTypeLabelKey], "adoption uses the VM's actual capacity type")
			require.Zero(t, adopted.insertions)

			absent := run(t, false, false, class)
			require.Nil(t, absent.nodeClaim)
			require.Error(t, absent.err)
			if readiness == "false" {
				require.True(t, karpcloudprovider.IsNodeClassNotReadyError(absent.err))
			} else {
				require.ErrorContains(t, absent.err, "Ready=Unknown")
			}
			require.Zero(t, absent.insertions)

			failedLookup := run(t, false, true, class)
			require.Nil(t, failedLookup.nodeClaim)
			require.ErrorContains(t, failedLookup.err, "adopting instance")
			require.Zero(t, failedLookup.insertions)
		})
	}
}
