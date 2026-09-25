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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
)

type fixedVersion struct{}

func (fixedVersion) Get(context.Context) (string, error) { return "v1.35.1", nil }
func (fixedVersion) Inject(string)                       {}

func TestImageReconcileCatalogQuotaPreservesStatus(t *testing.T) {
	for _, reason := range []string{"RATE_LIMIT_EXCEEDED", "forbidden"} {
		t.Run(reason, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
					"code": 403, "message": "quota", "errors": []map[string]string{{"reason": reason, "message": "quota"}},
				}})
			}))
			defer srv.Close()
			svc, err := compute.NewService(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
			require.NoError(t, err)
			reconciler := &Image{imageProvider: imagefamily.NewDefaultProvider(svc, fixedVersion{}, nil)}
			nc := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
				ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Alias: "ContainerOptimizedOS@latest"}},
			}}
			nc.Status.Images = []v1alpha1.Image{{SourceImage: "previous-image"}}
			nc.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
			result, err := reconciler.Reconcile(context.Background(), nc)
			if reason == "forbidden" {
				require.Error(t, err)
				require.Zero(t, result.RequeueAfter)
			} else {
				require.NoError(t, err)
				require.GreaterOrEqual(t, result.RequeueAfter, time.Minute)
				_, err = reconciler.Reconcile(context.Background(), nc)
				require.NoError(t, err)
				require.Equal(t, 1, calls, "watch-triggered reconcile must honor cooldown")
			}
			require.Equal(t, "previous-image", nc.Status.Images[0].SourceImage)
			require.True(t, nc.StatusConditions().IsTrue(v1alpha1.ConditionTypeImagesReady))
		})
	}
}
