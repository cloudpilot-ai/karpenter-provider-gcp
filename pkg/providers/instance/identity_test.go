/*
Copyright 2026 The Karpenter GCP Authors.

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
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

const (
	identityPath = "/projects/test-project/zones/us-central1-a/instances/karpenter-node"
	// The attestation key is read by the verified numeric incarnation, not the name.
	shieldedIdentityPath  = "/projects/test-project/zones/us-central1-a/instances/987654321/getShieldedInstanceIdentity"
	attestationKeyPEMBody = "-----BEGIN PUBLIC KEY-----\ncGVt\n-----END PUBLIC KEY-----\n"
)

func writeIdentityInstance(w http.ResponseWriter, numericID uint64) {
	writeJSON(w, &compute.Instance{Name: "karpenter-node", Zone: "zones/us-central1-a", Id: numericID})
}

func writeShieldedIdentity(w http.ResponseWriter) {
	writeJSON(w, &compute.ShieldedInstanceIdentity{
		SigningKey: &compute.ShieldedInstanceIdentityEntry{EkPub: attestationKeyPEMBody},
	})
}

// identityProviderDouble serves the Compute calls GetIdentity makes.
func identityProviderDouble(t *testing.T, handler http.HandlerFunc) *DefaultProvider {
	t.Helper()
	provider := newFakeComputeProvider(t, handler)
	provider.gkeProvider = &fakeGKEProvider{projectNumber: 123456789}
	return provider
}

func serveIdentity(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case identityPath:
		writeIdentityInstance(w, 987654321)
	case shieldedIdentityPath:
		writeShieldedIdentity(w)
	default:
		http.NotFound(w, r)
	}
}

func serveIdentityWithoutSigningKey(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != identityPath {
		if r.URL.Path == shieldedIdentityPath {
			writeJSON(w, &compute.ShieldedInstanceIdentity{
				EccP256SigningKey: &compute.ShieldedInstanceIdentityEntry{EkPub: attestationKeyPEMBody},
			})
			return
		}
		http.NotFound(w, r)
		return
	}
	writeIdentityInstance(w, 987654321)
}

// serveIdentityKeyReadByNameFails serves the identity by name only, so a
// name-addressed key read fails this case.
func serveIdentityKeyReadByNameFails(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case identityPath:
		writeIdentityInstance(w, 987654321)
	case shieldedIdentityPath:
		writeShieldedIdentity(w)
	case identityPath + "/getShieldedInstanceIdentity":
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, &googleapi.Error{Code: http.StatusInternalServerError, Message: "name-addressed identity read"})
	default:
		http.NotFound(w, r)
	}
}

func serveIdentityTransientFailure(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == identityPath {
		writeIdentityInstance(w, 987654321)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	writeJSON(w, &googleapi.Error{Code: http.StatusServiceUnavailable, Message: "backend unavailable"})
}

func serveNoInstance(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	writeJSON(w, &googleapi.Error{Code: http.StatusNotFound, Message: "not found"})
}

func TestGetIdentity(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		handler http.HandlerFunc
		// wantVM reports whether the lookup must return evidence.
		wantVM  bool
		wantErr bool
	}{
		{name: "returns the live VM identity and attestation key", handler: serveIdentity, wantVM: true},
		{name: "attestation key is read by the verified incarnation, not the name", handler: serveIdentityKeyReadByNameFails, wantVM: true},
		{name: "missing VM returns no identity", handler: serveNoInstance},
		{name: "VM without an RSA attestation key is an error", handler: serveIdentityWithoutSigningKey, wantErr: true},
		{name: "temporary Compute failure is reported", handler: serveIdentityTransientFailure, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := identityProviderDouble(t, tt.handler)

			identity, err := provider.GetIdentity(context.Background(), "us-central1-a", "karpenter-node")
			if tt.wantErr {
				require.Error(t, err)
				require.Nil(t, identity)
				return
			}
			require.NoError(t, err)
			if !tt.wantVM {
				require.Nil(t, identity)
				return
			}
			require.Equal(t, "test-project", identity.ProjectID)
			require.Equal(t, uint64(123456789), identity.ProjectNumber)
			require.Equal(t, "us-central1-a", identity.Zone)
			require.Equal(t, "karpenter-node", identity.Name)
			require.Equal(t, uint64(987654321), identity.NumericID)
			require.Equal(t, attestationKeyPEMBody, identity.AttestationKeyPEM)
		})
	}
}

// TestGetIdentityRejectsAReplacedVM models a VM deleted and recreated under the
// same name between the identity read and the attestation-key read.
func TestGetIdentityRejectsAReplacedVM(t *testing.T) {
	t.Parallel()

	incarnations := []uint64{111, 222, 333}
	reads := 0
	provider := identityProviderDouble(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == identityPath:
			reads++
			writeIdentityInstance(w, incarnations[reads-1])
		case strings.HasPrefix(r.URL.Path, "/projects/test-project/zones/us-central1-a/instances/") &&
			strings.HasSuffix(r.URL.Path, "/getShieldedInstanceIdentity"):
			// Every incarnation answers its own ID-addressed key read.
			writeShieldedIdentity(w)
		default:
			http.NotFound(w, r)
		}
	})

	identity, err := provider.GetIdentity(context.Background(), "us-central1-a", "karpenter-node")
	require.Error(t, err)
	require.Nil(t, identity)
}

// TestGetIdentityReadsComputeNotTheInstanceCache guards the security lookup: a
// stale name-keyed cache entry must not satisfy an attestation identity request.
func TestGetIdentityReadsComputeNotTheInstanceCache(t *testing.T) {
	t.Parallel()

	requests := 0
	provider := identityProviderDouble(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case identityPath:
			writeIdentityInstance(w, 987654321)
		case shieldedIdentityPath:
			writeShieldedIdentity(w)
		default:
			http.NotFound(w, r)
		}
	})
	provider.instanceCache.Set("karpenter-node", &Instance{Name: "karpenter-node", NumericID: 1}, 0)

	identity, err := provider.GetIdentity(context.Background(), "us-central1-a", "karpenter-node")
	require.NoError(t, err)
	require.Equal(t, uint64(987654321), identity.NumericID)
	require.Equal(t, 3, requests, "both VM reads and the identity read must hit the Compute API")
}
