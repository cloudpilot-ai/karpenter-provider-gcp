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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func TestValidateNodeClaimAuthorization(t *testing.T) {
	const nodeName = "karpenter-target"
	for _, tt := range []struct {
		name    string
		renewal bool
		mutate  func(*karpv1.NodeClaim)
		approve bool
	}{
		{
			name:   "bootstrap requires a launched claim",
			mutate: func(nc *karpv1.NodeClaim) { nc.Status.Conditions = nil },
		},
		{
			name: "bootstrap rejects a deleting claim",
			mutate: func(nc *karpv1.NodeClaim) {
				now := metav1.Now()
				nc.DeletionTimestamp = &now
			},
		},
		{
			name:   "bootstrap rejects another provider",
			mutate: func(nc *karpv1.NodeClaim) { nc.Spec.NodeClassRef.Group = "another.provider.example" },
		},
		{
			name:   "bootstrap rejects another node class kind",
			mutate: func(nc *karpv1.NodeClaim) { nc.Spec.NodeClassRef.Kind = "OtherNodeClass" },
		},
		{
			name:   "bootstrap requires a node class reference",
			mutate: func(nc *karpv1.NodeClaim) { nc.Spec.NodeClassRef = nil },
		},
		{
			name:   "bootstrap rejects an incomplete GCE identity",
			mutate: func(nc *karpv1.NodeClaim) { nc.Status.ProviderID = "gce:///us-central1-a/" + nodeName },
		},
		{
			name:    "renewal requires completed registration",
			renewal: true,
			mutate:  func(nc *karpv1.NodeClaim) { nc.Status.Conditions = nil },
		},
		{
			name:    "renewal rejects another provider",
			renewal: true,
			mutate:  func(nc *karpv1.NodeClaim) { nc.Spec.NodeClassRef.Group = "another.provider.example" },
		},
		{
			name:    "renewal requires the same GCE instance name",
			renewal: true,
			mutate:  func(nc *karpv1.NodeClaim) { nc.Status.ProviderID = "gce://my-project/us-central1-a/another-node" },
		},
		{
			name:    "registered renewal does not depend on launch condition",
			renewal: true,
			mutate: func(nc *karpv1.NodeClaim) {
				nc.Status.Conditions = nil
				nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
			},
			approve: true,
		},
		{
			name:    "registered nodes can renew while draining",
			renewal: true,
			mutate: func(nc *karpv1.NodeClaim) {
				now := metav1.Now()
				nc.DeletionTimestamp = &now
			},
			approve: true,
		},
	} {
		usernames := []string{"kubelet-nodepool-bootstrap", "kubelet-bootstrap"}
		if tt.renewal {
			usernames = []string{"system:node:" + nodeName}
		}
		for _, username := range usernames {
			t.Run(tt.name+"/"+username, func(t *testing.T) {
				nc := launchedNodeClaim("target", nodeName)
				request := bootstrapCSR(nodeRequest(t, nodeName))
				if tt.renewal {
					nc = registeredNodeClaim("target", nodeName)
					request = nodeCSR(nodeRequest(t, nodeName), nodeName)
				}
				request.Spec.Username = username
				tt.mutate(&nc)
				err := validateKubeletClientCSR(request, []karpv1.NodeClaim{nc})
				if tt.approve {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}

func TestReconcileCSRApproval(t *testing.T) {
	const nodeName = "karpenter-target"
	for _, tt := range []struct {
		name    string
		csr     func(*testing.T) *certv1.CertificateSigningRequest
		approve bool
	}{
		{
			name: "valid bootstrap",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, "karpenter-pending"))
			},
			approve: true,
		},
		{
			name: "GKE TPM bootstrap requester with only the authenticated group",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return kubeletCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-pending", Organization: []string{"system:nodes"}},
				}), "kubelet-bootstrap", "system:authenticated")
			},
			approve: true,
		},
		{
			name: "GKE TPM bootstrap requester cannot claim a registered node",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return kubeletCSR(nodeRequest(t, nodeName), "kubelet-bootstrap", nodePoolOrg, "system:authenticated")
			},
		},
		{
			name: "valid self renewal",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, nodeName), nodeName)
			},
			approve: true,
		},
		{
			name: "service account cannot claim a managed node",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return kubeletCSR(nodeRequest(t, nodeName), "system:serviceaccount:default:csr-creator", nodePoolOrg)
			},
		},
		{
			name: "node cannot claim another managed node",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, nodeName), "another-node")
			},
		},
		{
			name: "node shaped username without authenticated node group",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return kubeletCSR(nodeRequest(t, nodeName), "system:node:"+nodeName, nodePoolOrg, instanceOrg, "system:authenticated")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			request := tt.csr(t)
			pending := launchedNodeClaim("pending", "karpenter-pending")
			registered := registeredNodeClaim("target", nodeName)
			api, approvals := approvalClient(t)
			controller := NewController(api)
			controller.client = &csrReadClient{request: request, claims: []karpv1.NodeClaim{pending, registered}}

			_, err := controller.Reconcile(ctx)
			require.NoError(t, err)
			if tt.approve {
				require.Len(t, approvals, 1)
				got := <-approvals
				require.Len(t, got.Status.Conditions, 1)
				require.Equal(t, certv1.CertificateApproved, got.Status.Conditions[0].Type)
				require.Equal(t, "True", string(got.Status.Conditions[0].Status))
				require.Equal(t, "KarpenterAutoApprover", got.Status.Conditions[0].Reason)
			} else {
				require.Empty(t, approvals)
			}
		})
	}
}

func TestReconcileDoesNotReapproveDecidedRequests(t *testing.T) {
	for _, condition := range []certv1.RequestConditionType{certv1.CertificateApproved, certv1.CertificateDenied} {
		t.Run(string(condition), func(t *testing.T) {
			request := bootstrapCSR(nodeRequest(t, "karpenter-pending"))
			request.Status.Conditions = []certv1.CertificateSigningRequestCondition{{Type: condition, Status: "True", Reason: "ExistingDecision"}}
			pending := launchedNodeClaim("pending", "karpenter-pending")
			api, approvals := approvalClient(t)
			controller := NewController(api)
			controller.client = &csrReadClient{request: request, claims: []karpv1.NodeClaim{pending}}

			_, err := controller.Reconcile(context.Background())
			require.NoError(t, err)
			require.Empty(t, approvals)
		})
	}
}

func TestReconcileFailsClosedWhenClaimsCannotBeRead(t *testing.T) {
	request := bootstrapCSR(nodeRequest(t, "karpenter-pending"))
	api, approvals := approvalClient(t)
	readErr := errors.New("nodeclaim read unavailable")
	controller := NewController(api)
	controller.client = &csrReadClient{request: request, claimError: readErr}

	_, err := controller.Reconcile(context.Background())
	require.ErrorIs(t, err, readErr)
	require.Empty(t, approvals)
}

type csrReadClient struct {
	client.Client
	request    *certv1.CertificateSigningRequest
	claims     []karpv1.NodeClaim
	claimError error
}

func (c *csrReadClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch objects := list.(type) {
	case *certv1.CertificateSigningRequestList:
		objects.Items = []certv1.CertificateSigningRequest{*c.request.DeepCopy()}
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

func approvalClient(t *testing.T) (kubernetes.Interface, <-chan certv1.CertificateSigningRequest) {
	t.Helper()
	approvals := make(chan certv1.CertificateSigningRequest, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/apis/certificates.k8s.io/v1/certificatesigningrequests/csr-test/approval" {
			t.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var request certv1.CertificateSigningRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decoding approval: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		approvals <- request
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(&request); err != nil {
			t.Errorf("encoding approval response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	api, err := kubernetes.NewForConfig(&rest.Config{
		Host:          server.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	})
	require.NoError(t, err)
	return api, approvals
}
