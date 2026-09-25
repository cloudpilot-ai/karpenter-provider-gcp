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

package imagefamily

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func TestCatalogQuotaCooldownAcrossSelectors(t *testing.T) {
	now := time.Now()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/images") {
			_ = json.NewEncoder(w).Encode(&compute.Image{Name: "image", Status: "READY", Architecture: OSArchitectureX86})
			return
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": 403, "message": "quota", "errors": []map[string]string{{"reason": "RATE_LIMIT_EXCEEDED", "message": "quota"}},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(&compute.ImageList{Items: []*compute.Image{
			{Name: "gke-1351-gke1396004-cos-125-19216-104-126-c-pre", CreationTimestamp: "2026-04-01T00:00:00Z", Status: "READY"},
		}})
	}))
	defer srv.Close()
	p := NewDefaultProvider(buildComputeService(t, srv), &fakeVersionProvider{version: "v1.35.1"}, nil)
	cos := p.providers[v1alpha1.ImageFamilyContainerOptimizedOS].(*ContainerOptimizedOS)
	cos.cooldown.now = func() time.Time { return now }
	alias := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@latest"}}}}
	family := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Version: "latest"}}}}

	_, err := p.List(context.Background(), alias)
	var apiErr *googleapi.Error
	require.ErrorAs(t, err, &apiErr)
	delay, limited := CatalogRateLimitRetryAfter(err)
	require.True(t, limited)
	require.GreaterOrEqual(t, delay, time.Minute)
	require.Less(t, delay, time.Minute+10*time.Second)

	_, err = p.List(context.Background(), family)
	require.Error(t, err)
	_, limited = CatalogRateLimitRetryAfter(err)
	require.True(t, limited)
	require.Equal(t, 1, calls, "another selector must not call the API during cooldown")
	_, err = p.List(context.Background(), &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{ID: "projects/gke-node-images/global/images/custom"}},
	}})
	require.NoError(t, err, "an exact image GET must not be blocked")

	now = now.Add(2 * time.Minute)
	_, err = p.List(context.Background(), alias)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestCatalogRateLimitClassification(t *testing.T) {
	for _, reason := range []string{"rateLimitExceeded", "RATE_LIMIT_EXCEEDED"} {
		require.True(t, isCatalogQuotaError(&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: reason}}}))
	}
	for _, err := range []error{
		&googleapi.Error{Code: 403, Message: "RATE_LIMIT_EXCEEDED"},
		&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "forbidden"}}},
		&googleapi.Error{Code: 500, Errors: []googleapi.ErrorItem{{Reason: "RATE_LIMIT_EXCEEDED"}}},
		errors.New("RATE_LIMIT_EXCEEDED"),
	} {
		require.False(t, isCatalogQuotaError(err))
	}
}

func BenchmarkScanImageCatalog(b *testing.B) {
	const pages = 20
	const pageSize = 500
	images := make([][]*compute.Image, pages)
	for page := range images {
		images[page] = make([]*compute.Image, pageSize)
		for i := range images[page] {
			images[page][i] = &compute.Image{Name: "other-image-" + strconv.Itoa(page*pageSize+i), Status: "READY", CreationTimestamp: "2026-04-01T00:00:00Z"}
		}
	}
	images[0][0].Name = "matching-image"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 0
		if token := r.URL.Query().Get("pageToken"); token != "" {
			page, _ = strconv.Atoi(token)
		}
		response := &compute.ImageList{Items: images[page]}
		if page+1 < pages {
			response.NextPageToken = strconv.Itoa(page + 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	service, err := compute.NewService(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		stop bool
	}{{"newest", true}, {"full-scan-10000", false}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				err := scanImageCatalog(context.Background(), service, cosImageProject, nil, func(img *compute.Image) bool {
					return tc.stop && img.Name == "matching-image"
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestScanImageCatalogReturnsLaterPageError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_ = json.NewEncoder(w).Encode(&compute.ImageList{NextPageToken: "next", Items: []*compute.Image{{Name: "unrelated"}}})
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	err := scanImageCatalog(context.Background(), buildComputeService(t, srv), cosImageProject, nil, func(*compute.Image) bool { return false })
	require.Error(t, err)
	require.Equal(t, 2, calls)
}
