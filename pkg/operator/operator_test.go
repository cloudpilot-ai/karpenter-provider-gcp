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

package operator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

func TestComputeDefaultServiceAccountReturnsLookupError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "project unavailable", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	client, err := compute.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	require.NoError(t, err)
	_, err = getComputeDefaultServiceAccount(context.Background(), client, "project", "")
	require.Error(t, err)
}

func TestComputeDefaultServiceAccountLookup(t *testing.T) {
	t.Parallel()
	for _, account := range []string{"123-compute@developer.gserviceaccount.com", ""} {
		t.Run(account, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/projects/project", r.URL.Path)
				_, _ = fmt.Fprintf(w, `{"defaultServiceAccount":%q}`, account)
			}))
			t.Cleanup(server.Close)
			client, err := compute.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
			require.NoError(t, err)
			resolved, err := getComputeDefaultServiceAccount(context.Background(), client, "project", "")
			require.NoError(t, err)
			require.Equal(t, account, resolved)
		})
	}
}

func TestComputeDefaultServiceAccountSkipsLookupWhenConfigured(t *testing.T) {
	t.Parallel()
	// No Compute client is needed when the controller account is configured.
	account, err := getComputeDefaultServiceAccount(context.Background(), nil, "project", "controller@example.com")
	require.NoError(t, err)
	require.Empty(t, account, "the instance provider already receives the configured controller account separately")
}
