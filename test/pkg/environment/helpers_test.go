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

package environment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

func TestNewestImagePagesWithoutFilter(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Has("filter") || q.Get("orderBy") != "creationTimestamp desc" || q.Get("fields") == "" {
			http.Error(w, "unexpected list parameters", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if q.Get("pageToken") == "" {
			_ = json.NewEncoder(w).Encode(&compute.ImageList{NextPageToken: "next", Items: []*compute.Image{{Name: "unrelated", Status: "READY"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(&compute.ImageList{NextPageToken: "ignored", Items: []*compute.Image{{Name: "matching", Status: "READY"}}})
	}))
	defer srv.Close()
	svc, err := compute.NewService(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	img, err := newestImage(context.Background(), svc, "gke-node-images", func(img *compute.Image) bool { return img.Name == "matching" })
	require.NoError(t, err)
	require.Equal(t, "matching", img.Name)
	require.Equal(t, 2, calls)
}
