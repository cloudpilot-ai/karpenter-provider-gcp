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

package csr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	certv1 "k8s.io/api/certificates/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/instance"
)

func TestReconcileDoesNotReapproveDecidedRequests(t *testing.T) {
	for _, tt := range []struct {
		name    string
		decided func(*certv1.CertificateSigningRequest)
	}{
		{
			name: "approved",
			decided: func(request *certv1.CertificateSigningRequest) {
				request.Status.Conditions = []certv1.CertificateSigningRequestCondition{{Type: certv1.CertificateApproved, Status: "True", Reason: "ExistingDecision"}}
			},
		},
		{
			name: "denied",
			decided: func(request *certv1.CertificateSigningRequest) {
				request.Status.Conditions = []certv1.CertificateSigningRequestCondition{{Type: certv1.CertificateDenied, Status: "True", Reason: "ExistingDecision"}}
			},
		},
		{
			// A request that already holds a certificate must never be approved again,
			// even when no terminal condition was recorded.
			name: "already issued",
			decided: func(request *certv1.CertificateSigningRequest) {
				request.Status.Certificate = []byte("issued-certificate")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newAttestationFixture(t)
			request := bootstrapCSR(encodePEMBlocks(fixture.blocks(t)))
			tt.decided(request)
			api, double := newKubeAPIDouble(t)
			double.sarAllowed = true
			controller := newTestController(t, api, identityProviderFunc(func(context.Context, string, string) (*instance.InstanceIdentity, error) {
				return fixtureIdentity(t, fixture), nil
			}))
			controller.client = &csrReadClient{request: request, claims: []karpv1.NodeClaim{claimForFixture(t, "pending")}}

			_, err := controller.Reconcile(context.Background())
			require.NoError(t, err)
			require.Empty(t, double.recordedApprovals())
			require.Empty(t, double.recordedReviews(), "a decided request must not even be authorized")
		})
	}
}

func TestReconcileFailsClosedWhenClaimsCannotBeRead(t *testing.T) {
	fixture := newAttestationFixture(t)
	api, double := newKubeAPIDouble(t)
	readErr := errors.New("nodeclaim read unavailable")
	controller := newTestController(t, api, identityProviderFunc(func(context.Context, string, string) (*instance.InstanceIdentity, error) {
		return fixtureIdentity(t, fixture), nil
	}))
	controller.client = &csrReadClient{request: bootstrapCSR(encodePEMBlocks(fixture.blocks(t))), claimError: readErr}

	_, err := controller.Reconcile(context.Background())
	require.ErrorIs(t, err, readErr)
	require.Empty(t, double.recordedApprovals())
}

func TestReconcileContinuesAfterTemporaryIdentityFailure(t *testing.T) {
	fixture := newAttestationFixture(t)
	failing := bootstrapCSR(encodePEMBlocks(fixture.blocks(t)))
	failing.Name = "csr-failing"
	nodeName := "karpenter-other"
	other := registeredNodeClaim("other", nodeName, 42)
	selfRenewal := nodeCSR(nodeRequest(t, nodeName), nodeName)
	selfRenewal.Name = "csr-renewal"

	api, double := newKubeAPIDouble(t)
	double.sarAllowed = true
	controller := newTestController(t, api, identityProviderFunc(func(context.Context, string, string) (*instance.InstanceIdentity, error) {
		return nil, errors.New("compute unavailable")
	}))
	controller.client = &csrReadClient{requests: []*certv1.CertificateSigningRequest{failing, selfRenewal}, claims: []karpv1.NodeClaim{claimForFixture(t, "pending"), other}}

	_, err := controller.Reconcile(context.Background())
	require.Error(t, err, "the temporary identity failure is reported for retry")

	approvals := double.recordedApprovals()
	require.Len(t, approvals, 1, "the unrelated request must still be approved")
	require.Equal(t, "csr-renewal", approvals[0].Name)
}

func TestReconcileReportsApprovalConflict(t *testing.T) {
	fixture := newAttestationFixture(t)
	api, double := newKubeAPIDouble(t)
	double.sarAllowed = true
	double.approvalConflict = true
	controller := newTestController(t, api, identityProviderFunc(func(context.Context, string, string) (*instance.InstanceIdentity, error) {
		return fixtureIdentity(t, fixture), nil
	}))
	controller.client = &csrReadClient{request: bootstrapCSR(encodePEMBlocks(fixture.blocks(t))), claims: []karpv1.NodeClaim{claimForFixture(t, "pending")}}

	_, err := controller.Reconcile(context.Background())
	require.Error(t, err, "a lost approval race is reported and retried against the reloaded request")
	require.Empty(t, double.recordedApprovals())
}

type csrReadClient struct {
	client.Client
	request    *certv1.CertificateSigningRequest
	requests   []*certv1.CertificateSigningRequest
	claims     []karpv1.NodeClaim
	claimError error
}

func (c *csrReadClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch objects := list.(type) {
	case *certv1.CertificateSigningRequestList:
		requests := c.requests
		if c.request != nil {
			requests = []*certv1.CertificateSigningRequest{c.request}
		}
		for _, request := range requests {
			objects.Items = append(objects.Items, *request.DeepCopy())
		}
	case *karpv1.NodeClaimList:
		if c.claimError != nil {
			return c.claimError
		}
		objects.Items = c.claims
	default:
		return fmt.Errorf("unexpected list type %T", list)
	}
	return nil
}

// kubeAPIDouble serves the CSR approval and SubjectAccessReview endpoints the
// controller calls and records what it received.
type kubeAPIDouble struct {
	mu               sync.Mutex
	reviews          []authorizationv1.SubjectAccessReview
	approvals        []certv1.CertificateSigningRequest
	sarAllowed       bool
	sarFailure       bool
	approvalConflict bool
}

func (d *kubeAPIDouble) recordedReviews() []authorizationv1.SubjectAccessReview {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]authorizationv1.SubjectAccessReview(nil), d.reviews...)
}

func (d *kubeAPIDouble) recordedApprovals() []certv1.CertificateSigningRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]certv1.CertificateSigningRequest(nil), d.approvals...)
}

func newKubeAPIDouble(t *testing.T) (kubernetes.Interface, *kubeAPIDouble) {
	t.Helper()
	double := &kubeAPIDouble{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/apis/authorization.k8s.io/v1/subjectaccessreviews":
			if double.sarFailure {
				http.Error(w, "subject access review unavailable", http.StatusInternalServerError)
				return
			}
			var review authorizationv1.SubjectAccessReview
			if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
				t.Errorf("decoding subject access review: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			double.mu.Lock()
			double.reviews = append(double.reviews, review)
			double.mu.Unlock()
			review.Status.Allowed = double.sarAllowed
			writeJSON(t, w, &review)
		case r.Method == http.MethodPut && r.URL.Path == "/apis/certificates.k8s.io/v1/certificatesigningrequests/csr-test/approval",
			r.Method == http.MethodPut && len(r.URL.Path) > len("/approval") && r.URL.Path[len(r.URL.Path)-len("/approval"):] == "/approval":
			if double.approvalConflict {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"the object has been modified","reason":"Conflict","code":409}`))
				return
			}
			var request certv1.CertificateSigningRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decoding approval: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			double.mu.Lock()
			double.approvals = append(double.approvals, request)
			double.mu.Unlock()
			writeJSON(t, w, &request)
		default:
			t.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	api, err := kubernetes.NewForConfig(&rest.Config{
		Host:          server.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	})
	require.NoError(t, err)
	return api, double
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encoding response: %v", err)
	}
}
