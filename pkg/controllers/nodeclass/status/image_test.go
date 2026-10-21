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

package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	statuspkg "github.com/awslabs/operatorpkg/status"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
)

// fakeImageProvider returns a fixed resolution result. Resolving images is the image provider's
// contract and is covered in pkg/providers/imagefamily; these cases stay on how the reconciler
// publishes that result.
type fakeImageProvider struct {
	images imagefamily.Images
	err    error
}

func (f fakeImageProvider) List(context.Context, *v1alpha1.GCENodeClass) (imagefamily.Images, error) {
	return f.images, f.err
}

func amd64Image(sourceImage string) imagefamily.Image {
	return imagefamily.Image{
		SourceImage: sourceImage,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, imagefamily.OSArchAMD64Requirement),
		),
	}
}

func nvidiaImage(sourceImage string) imagefamily.Image {
	return imagefamily.Image{
		SourceImage: sourceImage,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, imagefamily.OSArchAMD64Requirement),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpExists),
		),
	}
}

func cosLatestNodeClass() *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@latest"}},
	}}
}

// staleStatus is a successfully published status from an earlier selector. It must never be a
// launch fallback once resolution starts failing.
func staleStatus(nodeClass *v1alpha1.GCENodeClass) {
	nodeClass.Status.Images = []v1alpha1.Image{{SourceImage: "projects/gke-node-images/global/images/stale"}}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
}

func requireImagesNotReady(t *testing.T, nodeClass *v1alpha1.GCENodeClass, reason string) {
	t.Helper()
	condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady)
	require.False(t, condition.IsTrue(), "ImagesReady condition: %+v", condition)
	require.Equal(t, reason, condition.Reason)
	require.NotEmpty(t, condition.Message)
	require.False(t, nodeClass.StatusConditions().Get(statuspkg.ConditionReady).IsTrue(),
		"a NodeClass without ready images must not be Ready, or it would be launchable")
}

func TestReconcilePublishesOnlyUsableImages(t *testing.T) {
	t.Parallel()

	// A partial COS set: the arm64 variant is not published, so only amd64 CPU and NVIDIA appear.
	nodeClass := cosLatestNodeClass()
	reconciler := &Image{imageProvider: fakeImageProvider{images: imagefamily.Images{
		amd64Image("projects/gke-node-images/global/images/amd64"),
		nvidiaImage("projects/gke-node-images/global/images/nvidia"),
	}}}

	result, err := reconciler.Reconcile(context.Background(), nodeClass)

	require.NoError(t, err)
	require.Equal(t, reconcile.Result{RequeueAfter: 5 * time.Minute}, result)
	require.Equal(t, []v1alpha1.Image{
		{
			SourceImage: "projects/gke-node-images/global/images/amd64",
			Requirements: []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{imagefamily.OSArchAMD64Requirement}},
				{Key: v1alpha1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpDoesNotExist},
			},
		},
		{
			SourceImage: "projects/gke-node-images/global/images/nvidia",
			Requirements: []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{imagefamily.OSArchAMD64Requirement}},
				{Key: v1alpha1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpExists},
			},
		},
	}, nodeClass.Status.Images, "the published images keep their own requirements in a stable key order")
	require.True(t, nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).IsTrue(),
		"at least one usable COS variant makes the images ready")
}

func TestReconcileUnchangedImagesDoNotChurnStatus(t *testing.T) {
	t.Parallel()

	nodeClass := cosLatestNodeClass()
	reconciler := &Image{imageProvider: fakeImageProvider{images: imagefamily.Images{
		amd64Image("projects/gke-node-images/global/images/amd64"),
	}}}

	_, err := reconciler.Reconcile(context.Background(), nodeClass)
	require.NoError(t, err)
	images := append([]v1alpha1.Image(nil), nodeClass.Status.Images...)
	transition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).LastTransitionTime

	_, err = reconciler.Reconcile(context.Background(), nodeClass)
	require.NoError(t, err)

	require.Equal(t, images, nodeClass.Status.Images)
	require.Equal(t, transition, nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).LastTransitionTime,
		"an unchanged image set must not re-transition the condition")
}

func TestReconcileEmptyResolutionClearsImagesAndRetries(t *testing.T) {
	t.Parallel()

	nodeClass := cosLatestNodeClass()
	staleStatus(nodeClass)
	reconciler := &Image{imageProvider: fakeImageProvider{}}

	result, err := reconciler.Reconcile(context.Background(), nodeClass)

	require.NoError(t, err)
	require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result, "an empty resolution must retry instead of waiting for an event")
	require.Empty(t, nodeClass.Status.Images, "a confirmed empty match must clear the previous images")
	requireImagesNotReady(t, nodeClass, imagesNotFoundReason)
}

func TestReconcileLookupFailureKeepsImagesDiagnosticOnly(t *testing.T) {
	t.Parallel()

	nodeClass := cosLatestNodeClass()
	staleStatus(nodeClass)
	reconciler := &Image{imageProvider: fakeImageProvider{
		err: errors.New("googleapi: Error 403: Required permission compute.images.get"),
	}}

	result, err := reconciler.Reconcile(context.Background(), nodeClass)

	require.Error(t, err, "an infrastructure failure must keep controller-runtime backoff")
	require.Equal(t, reconcile.Result{}, result)
	requireImagesNotReady(t, nodeClass, imageLookupFailedReason)
	require.Len(t, nodeClass.Status.Images, 1, "previous images stay in status for diagnosis")
	require.Contains(t, nodeClass.Status.Images[0].SourceImage, "stale")
}

// The cases below run the real image provider so the reconciler is asserted against the typed
// errors production produces, not only against errors this package constructs. The provider is
// pointed at an empty Compute catalog, which is enough to reach the error classification:
// publication lag for a valid selector, and a configuration error for an invalid one.

func TestReconcileClassifiesUnpublishedImagesAndRecovers(t *testing.T) {
	t.Parallel()

	nodeClass := cosLatestNodeClass()
	reconciler := &Image{imageProvider: emptyCatalogImageProvider(t)}

	result, err := reconciler.Reconcile(context.Background(), nodeClass)

	require.NoError(t, err, "publication lag is not an infrastructure failure to back off")
	require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result, "an unpublished image must be retried shortly")
	require.Empty(t, nodeClass.Status.Images)
	requireImagesNotReady(t, nodeClass, imageUnavailableReason)

	// GCP publishes the amd64 variant and the next refresh resolves it.
	catalog := &catalogImageServer{images: []*compute.Image{{
		Name:   "gke-1351-gke1396004-cos-125-19216-104-126-c-pre",
		Status: "READY",
	}}}
	next := &Image{imageProvider: newCatalogImageProvider(t, catalog)}

	_, err = next.Reconcile(context.Background(), nodeClass)
	require.NoError(t, err)
	require.Len(t, nodeClass.Status.Images, 1)
	require.True(t, nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).IsTrue())
}

func TestReconcileUnavailableRefreshClearsPreviousImages(t *testing.T) {
	t.Parallel()

	// A previous refresh published a ready COS set.
	nodeClass := cosLatestNodeClass()
	published := &Image{imageProvider: fakeImageProvider{images: imagefamily.Images{
		amd64Image("projects/gke-node-images/global/images/amd64"),
		nvidiaImage("projects/gke-node-images/global/images/nvidia"),
	}}}
	_, err := published.Reconcile(context.Background(), nodeClass)
	require.NoError(t, err)
	require.Len(t, nodeClass.Status.Images, 2)
	require.True(t, nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).IsTrue())

	// The catalog no longer offers a usable COS variant.
	refreshed := &Image{imageProvider: emptyCatalogImageProvider(t)}
	result, err := refreshed.Reconcile(context.Background(), nodeClass)

	require.NoError(t, err, "confirmed unavailability is not an infrastructure failure")
	require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
	require.Empty(t, nodeClass.Status.Images, "confirmed unavailability must clear the previous images")
	requireImagesNotReady(t, nodeClass, imageUnavailableReason)
}

// A valid exact COS selector with no usable publication is a retried publication lag, not a
// configuration error and not an API failure that backs off.
func TestReconcileClassifiesUnavailableExactSelectors(t *testing.T) {
	t.Parallel()

	const build = "gke-1346-gke1068000-cos-125-19216-220-72-c-pre"
	exactBuild := func() *v1alpha1.GCENodeClass {
		return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@1.34.6-gke.1068000"}},
		}}
	}
	milestone := func() *v1alpha1.GCENodeClass {
		return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
			ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@125.19216.104.126"}},
		}}
	}

	tests := []struct {
		name      string
		nodeClass *v1alpha1.GCENodeClass
		catalog   *catalogImageServer
	}{
		{
			name:      "exact build has no published image",
			nodeClass: exactBuild(),
			catalog:   &catalogImageServer{},
		},
		{
			name:      "exact build image is still PENDING",
			nodeClass: exactBuild(),
			catalog:   &catalogImageServer{images: []*compute.Image{{Name: build, Status: "PENDING"}}},
		},
		{
			name:      "milestone pin has no published base image",
			nodeClass: milestone(),
			catalog:   &catalogImageServer{},
		},
		{
			name:      "every derived image of the exact build is unavailable",
			nodeClass: exactBuild(),
			// The base build is listed as READY but gone by the time it is looked up.
			catalog: &catalogImageServer{
				images:       []*compute.Image{{Name: build, Status: "READY"}},
				missingOnGet: []string{build},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reconciler := &Image{imageProvider: newCatalogImageProvider(t, tt.catalog)}

			result, err := reconciler.Reconcile(context.Background(), tt.nodeClass)

			require.NoError(t, err, "a valid selector without a usable publication must not return an error")
			require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
			require.Empty(t, tt.nodeClass.Status.Images)
			requireImagesNotReady(t, tt.nodeClass, imageUnavailableReason)
		})
	}
}

func TestReconcileClassifiesInvalidSelector(t *testing.T) {
	t.Parallel()

	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@125"}},
	}}
	reconciler := &Image{imageProvider: emptyCatalogImageProvider(t)}

	result, err := reconciler.Reconcile(context.Background(), nodeClass)

	require.NoError(t, err, "a configuration error is not retried with exponential backoff")
	require.Equal(t, reconcile.Result{RequeueAfter: time.Minute}, result)
	requireImagesNotReady(t, nodeClass, imageResolutionFailedReason)
}

// catalogImageServer is the minimal Compute API boundary the real image provider needs: a COS
// catalog list and a lookup per listed image. It is not a policy double: what the provider
// resolves from it is asserted in pkg/providers/imagefamily.
type catalogImageServer struct {
	images []*compute.Image
	// missingOnGet lists images the catalog still lists but no longer returns, modeling a
	// candidate that disappears between listing and lookup.
	missingOnGet []string
}

func newCatalogImageProvider(t *testing.T, catalog *catalogImageServer) imagefamily.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/images") {
			_ = json.NewEncoder(w).Encode(&compute.ImageList{Items: catalog.images})
			return
		}
		name := path.Base(r.URL.Path)
		for _, missing := range catalog.missingOnGet {
			if missing == name {
				http.Error(w, "image not found", http.StatusNotFound)
				return
			}
		}
		for _, image := range catalog.images {
			if image.Name == name {
				_ = json.NewEncoder(w).Encode(image)
				return
			}
		}
		http.Error(w, "image not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	service, err := compute.NewService(context.Background(),
		option.WithEndpoint(srv.URL+"/"),
		option.WithoutAuthentication(),
	)
	require.NoError(t, err)
	return imagefamily.NewDefaultProvider(service, fixedClusterVersion{}, nil)
}

func emptyCatalogImageProvider(t *testing.T) imagefamily.Provider {
	t.Helper()
	return newCatalogImageProvider(t, &catalogImageServer{})
}

type fixedClusterVersion struct{}

func (fixedClusterVersion) Get(context.Context) (string, error) { return "v1.35.1", nil }
