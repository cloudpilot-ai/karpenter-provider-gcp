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

package environment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
)

func TestGetGCEMachineType(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    map[string]int
		wantZones []string
		wantError string
	}{
		{
			name:      "uses cluster zones instead of other regional zones",
			status:    map[string]int{"region-a": http.StatusOK},
			wantZones: []string{"region-a"},
		},
		{
			name:      "continues past a zone missing the machine type",
			status:    map[string]int{"region-a": http.StatusNotFound, "region-b": http.StatusOK},
			wantZones: []string{"region-a", "region-b"},
		},
		{
			name:      "rejects absent catalog rather than passing a negative test",
			status:    map[string]int{"region-a": http.StatusNotFound, "region-b": http.StatusNotFound},
			wantZones: []string{"region-a", "region-b"},
			wantError: "not found in cluster zones",
		},
		{
			name:      "does not hide API errors by trying another zone",
			status:    map[string]int{"region-a": http.StatusForbidden, "region-b": http.StatusOK},
			wantZones: []string{"region-a"},
			wantError: "403",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const machineType = "c4-standard-4-lssd"
			var zones []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/projects/p/locations/region/clusters/cluster" {
					_ = json.NewEncoder(w).Encode(&container.Cluster{Locations: []string{"region-b", "region-a"}})
					return
				}
				for _, zone := range []string{"region-a", "region-b"} {
					if r.URL.Path != "/projects/p/zones/"+zone+"/machineTypes/"+machineType {
						continue
					}
					zones = append(zones, zone)
					status := tc.status[zone]
					w.WriteHeader(status)
					if status == http.StatusOK {
						_ = json.NewEncoder(w).Encode(&compute.MachineType{
							Name:             machineType,
							BundledLocalSsds: &compute.BundledLocalSsds{PartitionCount: 1},
						})
					}
					return
				}
				t.Errorf("unexpected request: %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			ctx := context.Background()
			computeSvc, err := compute.NewService(ctx, option.WithoutAuthentication(), option.WithEndpoint(srv.URL+"/"))
			require.NoError(t, err)
			containerSvc, err := container.NewService(ctx, option.WithoutAuthentication(), option.WithEndpoint(srv.URL+"/"))
			require.NoError(t, err)
			env := &Environment{
				ProjectID:       "p",
				ClusterName:     "cluster",
				ClusterLocation: "region",
				computeSvc:      computeSvc,
				containerSvc:    containerSvc,
			}

			mt, err := env.GetGCEMachineType(ctx, machineType)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Nil(t, mt)
			} else {
				require.NoError(t, err)
				require.Equal(t, machineType, mt.Name)
				require.Equal(t, int64(1), mt.BundledLocalSsds.PartitionCount)
			}
			require.Equal(t, tc.wantZones, zones)
		})
	}
}
