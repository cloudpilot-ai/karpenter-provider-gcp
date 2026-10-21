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

type fakeImageProvider struct {
	images imagefamily.Images
	err    error
}

func (f fakeImageProvider) List(context.Context, *v1alpha1.GCENodeClass) (imagefamily.Images, error) {
	return f.images, f.err
}

func TestReconcileImageFailures(t *testing.T) {
	t.Parallel()
	unavailable, invalid := imageResolutionErrors(t)
	lookup := errors.New("Compute API unavailable")
	previous := []v1alpha1.Image{{SourceImage: "projects/gke-node-images/global/images/previous"}}
	for _, tc := range []struct {
		name      string
		err       error
		reason    string
		keep      bool
		requeue   time.Duration
		returnErr error
	}{
		{"empty match", nil, "ImagesNotFound", false, time.Minute, nil},
		{"unavailable publication", unavailable, "ImageUnavailable", false, time.Minute, nil},
		{"invalid selector", invalid, "ImageResolutionFailed", true, time.Minute, nil},
		{"API failure", lookup, "ImageLookupFailed", true, 0, lookup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nodeClass := &v1alpha1.GCENodeClass{Status: v1alpha1.GCENodeClassStatus{Images: previous}}
			nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
			result, err := (&Image{imageProvider: fakeImageProvider{err: tc.err}}).Reconcile(context.Background(), nodeClass)

			require.ErrorIs(t, err, tc.returnErr)
			require.Equal(t, reconcile.Result{RequeueAfter: tc.requeue}, result)
			condition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady)
			require.True(t, condition.IsFalse())
			require.Equal(t, tc.reason, condition.Reason)
			require.NotEmpty(t, condition.Message)
			require.False(t, nodeClass.StatusConditions().IsTrue(statuspkg.ConditionReady))
			if tc.keep {
				require.Equal(t, previous, nodeClass.Status.Images, "retained images must remain diagnostic only")
			} else {
				require.Empty(t, nodeClass.Status.Images)
			}
		})
	}
}

func TestReconcileImagePublicationAndRecovery(t *testing.T) {
	t.Parallel()
	unavailable, _ := imageResolutionErrors(t)
	nodeClass := &v1alpha1.GCENodeClass{}
	reconciler := &Image{imageProvider: fakeImageProvider{err: unavailable}}
	_, err := reconciler.Reconcile(context.Background(), nodeClass)
	require.NoError(t, err)
	require.True(t, nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).IsFalse())

	// A partial set is valid, and requirements must survive publication in stable key order.
	const (
		source = "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre"
		gpu    = "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-nvda"
	)
	reconciler.imageProvider = fakeImageProvider{images: imagefamily.Images{
		{SourceImage: source, Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpDoesNotExist),
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"))},
		{SourceImage: gpu, Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpExists))},
	}}
	result, err := reconciler.Reconcile(context.Background(), nodeClass)
	require.NoError(t, err)
	require.Equal(t, reconcile.Result{RequeueAfter: 5 * time.Minute}, result)
	require.True(t, nodeClass.StatusConditions().IsTrue(v1alpha1.ConditionTypeImagesReady))
	want := []v1alpha1.Image{
		{SourceImage: source, Requirements: []corev1.NodeSelectorRequirement{
			{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
			{Key: v1alpha1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpDoesNotExist},
		}},
		{SourceImage: gpu, Requirements: []corev1.NodeSelectorRequirement{
			{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
			{Key: v1alpha1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpExists},
		}},
	}
	require.Equal(t, want, nodeClass.Status.Images)
	transition := nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).LastTransitionTime

	_, err = reconciler.Reconcile(context.Background(), nodeClass)
	require.NoError(t, err)
	require.Equal(t, want, nodeClass.Status.Images)
	require.Equal(t, transition, nodeClass.StatusConditions().Get(v1alpha1.ConditionTypeImagesReady).LastTransitionTime)
}

// Obtain representative real errors once; selector-specific cases belong to imagefamily tests.
func imageResolutionErrors(t *testing.T) (unavailable, invalid error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&compute.ImageList{})
	}))
	t.Cleanup(srv.Close)
	service, err := compute.NewService(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	provider := imagefamily.NewDefaultProvider(service, nil, nil)
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@latest"}},
	}}
	_, unavailable = provider.List(context.Background(), nodeClass)
	require.True(t, imagefamily.IsImageUnavailableError(unavailable))
	nodeClass.Spec.ImageSelectorTerms = []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@125"}}
	_, invalid = provider.List(context.Background(), nodeClass)
	require.True(t, imagefamily.IsImageResolutionError(invalid))
	return unavailable, invalid
}
