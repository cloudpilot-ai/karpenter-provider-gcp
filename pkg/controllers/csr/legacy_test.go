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
	"encoding/pem"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/instance"
)

const legacyClaimUID = types.UID("8d3b1a4e-1111-4222-8333-444444444444")

// legacyInstances serves the live VM incarnation. GetIdentity is the attested
// path's lookup and must never be used for a legacy request.
type legacyInstances struct {
	t           *testing.T
	incarnation *instance.InstanceIncarnation
	err         error
}

func (l *legacyInstances) GetIdentity(context.Context, string, string) (*instance.InstanceIdentity, error) {
	l.t.Error("legacy bootstrap must not read attestation identity")
	return nil, errors.New("unexpected attestation identity read")
}

func (l *legacyInstances) GetIncarnation(context.Context, string, string) (*instance.InstanceIncarnation, error) {
	return l.incarnation, l.err
}

// liveIncarnation is the VM that matches launchedLegacyClaim.
func liveIncarnation() *instance.InstanceIncarnation {
	return &instance.InstanceIncarnation{
		ProjectID:    "my-project",
		Zone:         attestationVM.Zone,
		Name:         attestationVM.Name,
		NumericID:    attestationVM.ID,
		NodeClaimUID: string(legacyClaimUID),
	}
}

// scriptedPolicy returns each scripted answer once, in order, and records the
// reads. Reading past the script is an error so a reused result would show.
type scriptedPolicy struct {
	mu      sync.Mutex
	answers []policyAnswer
	reads   int
}

type policyAnswer struct {
	enabled bool
	err     error
}

func (p *scriptedPolicy) ShieldedNodesEnabled(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.reads > len(p.answers) {
		return false, errors.New("policy read beyond the script")
	}
	answer := p.answers[p.reads-1]
	return answer.enabled, answer.err
}

func disabledPolicy() *scriptedPolicy { return &scriptedPolicy{answers: []policyAnswer{{}}} }

func launchedLegacyClaim() karpv1.NodeClaim {
	nc := launchedNodeClaim("pending", attestationVM.Name, attestationVM.ID)
	nc.UID = legacyClaimUID
	return nc
}

// legacyRequestPEM builds a certificate request in the shape the native kubelet
// sends: CN system:node:<name> and the single organization system:nodes.
func legacyRequestPEM(t *testing.T, mutate func(*x509.CertificateRequest)) []byte {
	t.Helper()
	template := &x509.CertificateRequest{Subject: pkix.Name{
		CommonName:   "system:node:" + attestationVM.Name,
		Organization: []string{nodesGroup},
	}}
	if mutate != nil {
		mutate(template)
	}
	return requestPEM(t, template)
}

// legacyCSR is a request as the native kubelet sends it: requester "kubelet" and
// the digital signature and client auth usages.
func legacyCSR(request []byte) *certv1.CertificateSigningRequest {
	csr := kubeletCSR(request, legacyBootstrapUsername, "system:authenticated")
	csr.Spec.Usages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageClientAuth}
	csr.Spec.UID = "8d3b1a4e-2222-4444-8888-000000000002"
	csr.Spec.Extra = map[string]certv1.ExtraValue{"authentication.kubernetes.io/credential-id": {"legacy-bootstrap"}}
	return csr
}

func TestLegacyBootstrapAuthorization(t *testing.T) {
	validRequest := legacyRequestPEM(t, nil)
	for _, tc := range []struct {
		name string
		// request overrides the request PEM; the default is a valid plain request.
		request func(t *testing.T) []byte
		csr     func(csr *certv1.CertificateSigningRequest)
		claim   func(nc *karpv1.NodeClaim)
		// claims overrides the listed NodeClaims.
		claims      func(nc karpv1.NodeClaim) []karpv1.NodeClaim
		incarnation func(*instance.InstanceIncarnation) (*instance.InstanceIncarnation, error)
		sarDenied   bool
		sarError    bool
		policy      []policyAnswer

		wantApproved   bool
		wantRetry      bool
		wantSARs       int
		wantPolicyRead int
	}{
		{
			name:           "native request on a cluster with Shielded Nodes disabled",
			wantApproved:   true,
			wantSARs:       1,
			wantPolicyRead: 1,
		},
		{
			name:    "request with surrounding whitespace and the upstream key encipherment usage",
			request: func(*testing.T) []byte { return append(append([]byte("\n\n"), validRequest...), " \t\r\n\n"...) },
			csr: func(csr *certv1.CertificateSigningRequest) {
				csr.Spec.Usages = append(csr.Spec.Usages, certv1.UsageKeyEncipherment)
			},
			wantApproved:   true,
			wantSARs:       1,
			wantPolicyRead: 1,
		},

		{
			name:           "request prefixed with a space",
			request:        func(*testing.T) []byte { return append([]byte(" "), validRequest...) },
			wantApproved:   true,
			wantSARs:       1,
			wantPolicyRead: 1,
		},
		{
			name:           "request prefixed with a tab",
			request:        func(*testing.T) []byte { return append([]byte("\t"), validRequest...) },
			wantApproved:   true,
			wantSARs:       1,
			wantPolicyRead: 1,
		},

		// Whole-envelope boundary: exactly one plain CSR.
		{
			name:    "leading non-PEM text before the request",
			request: func(*testing.T) []byte { return append([]byte("junk\n"), validRequest...) },
		},
		{
			name:    "trailing non-PEM text after the request",
			request: func(*testing.T) []byte { return append(append([]byte{}, validRequest...), "trailing junk\n"...) },
		},
		{
			name: "malformed leading PEM block before a valid request",
			request: func(*testing.T) []byte {
				return append([]byte("-----BEGIN CERTIFICATE REQUEST-----\n!!not base64!!\n-----END CERTIFICATE REQUEST-----\n"), validRequest...)
			},
		},
		{
			name:    "duplicate certificate request blocks",
			request: func(*testing.T) []byte { return append(append([]byte{}, validRequest...), validRequest...) },
		},
		{
			name: "extra PEM block after the request",
			request: func(*testing.T) []byte {
				return append(append([]byte{}, validRequest...), pem.EncodeToMemory(&pem.Block{Type: "VM IDENTITY", Bytes: []byte("{}")})...)
			},
		},
		{
			name: "TPM envelope sent under the legacy username",
			request: func(t *testing.T) []byte {
				return encodePEMBlocks(newAttestationFixture(t).blocks(t))
			},
		},

		// Strict subject and usage shape.
		{
			name:    "repeated system:nodes organization",
			request: func(t *testing.T) []byte { return legacyOrganizations(t, nodesGroup, nodesGroup) },
		},
		{
			name:    "node pool organization",
			request: func(t *testing.T) []byte { return legacyOrganizations(t, nodesGroup, nodePoolOrg) },
		},
		{
			name:    "instance organization",
			request: func(t *testing.T) []byte { return legacyOrganizations(t, nodesGroup, instanceOrg) },
		},
		{
			name:    "additional privileged organization",
			request: func(t *testing.T) []byte { return legacyOrganizations(t, nodesGroup, "system:masters") },
		},
		{
			name: "additional subject attribute",
			request: func(t *testing.T) []byte {
				return legacyRequestPEM(t, func(r *x509.CertificateRequest) { r.Subject.OrganizationalUnit = []string{"extra"} })
			},
		},
		{
			name: "client auth without digital signature",
			csr: func(csr *certv1.CertificateSigningRequest) {
				csr.Spec.Usages = []certv1.KeyUsage{certv1.UsageClientAuth}
			},
		},
		{
			name: "key encipherment without digital signature",
			csr: func(csr *certv1.CertificateSigningRequest) {
				csr.Spec.Usages = []certv1.KeyUsage{certv1.UsageKeyEncipherment, certv1.UsageClientAuth}
			},
		},
		{
			name: "duplicate usages",
			csr: func(csr *certv1.CertificateSigningRequest) {
				csr.Spec.Usages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageClientAuth, certv1.UsageClientAuth}
			},
		},
		{
			name: "server auth usage",
			csr: func(csr *certv1.CertificateSigningRequest) {
				csr.Spec.Usages = append(csr.Spec.Usages, certv1.UsageServerAuth)
			},
		},

		// NodeClaim lifecycle and association.
		{
			name:   "no NodeClaim for the node",
			claims: func(karpv1.NodeClaim) []karpv1.NodeClaim { return nil },
		},
		{
			name: "NodeClaim of another provider",
			claim: func(nc *karpv1.NodeClaim) {
				nc.Spec.NodeClassRef.Group = "karpenter.k8s.aws"
			},
		},
		{
			name:  "registered NodeClaim",
			claim: func(nc *karpv1.NodeClaim) { nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered) },
		},
		{
			name:  "NodeClaim that already has a node",
			claim: func(nc *karpv1.NodeClaim) { nc.Status.NodeName = attestationVM.Name },
		},
		{
			name: "deleting NodeClaim",
			claim: func(nc *karpv1.NodeClaim) {
				now := metav1.Now()
				nc.DeletionTimestamp = &now
			},
		},
		{
			name:  "unlaunched NodeClaim",
			claim: func(nc *karpv1.NodeClaim) { nc.StatusConditions().SetFalse(karpv1.ConditionTypeLaunched, "x", "x") },
		},
		{
			name:  "NodeClaim without a recorded VM incarnation",
			claim: func(nc *karpv1.NodeClaim) { delete(nc.Annotations, v1alpha1.AnnotationInstanceNumericID) },
		},
		{
			name:  "NodeClaim with a zero recorded VM incarnation",
			claim: func(nc *karpv1.NodeClaim) { nc.Annotations[v1alpha1.AnnotationInstanceNumericID] = "0" },
		},
		{
			name:  "NodeClaim without a complete provider ID",
			claim: func(nc *karpv1.NodeClaim) { nc.Status.ProviderID = "gce://my-project/" + attestationVM.Name },
		},
		{
			name: "replacement VM with another numeric ID",
			incarnation: func(live *instance.InstanceIncarnation) (*instance.InstanceIncarnation, error) {
				live.NumericID++
				return live, nil
			},
		},
		{
			name: "VM in another project",
			incarnation: func(live *instance.InstanceIncarnation) (*instance.InstanceIncarnation, error) {
				live.ProjectID = "other-project"
				return live, nil
			},
		},
		{
			name: "VM without the NodeClaim ownership label",
			incarnation: func(live *instance.InstanceIncarnation) (*instance.InstanceIncarnation, error) {
				live.NodeClaimUID = ""
				return live, nil
			},
		},
		{
			name: "VM owned by another NodeClaim UID",
			incarnation: func(live *instance.InstanceIncarnation) (*instance.InstanceIncarnation, error) {
				live.NodeClaimUID = "another-uid"
				return live, nil
			},
		},
		{
			name:  "NodeClaim without a UID",
			claim: func(nc *karpv1.NodeClaim) { nc.UID = "" },
			incarnation: func(live *instance.InstanceIncarnation) (*instance.InstanceIncarnation, error) {
				live.NodeClaimUID = ""
				return live, nil
			},
		},
		{
			name: "VM no longer exists",
			incarnation: func(*instance.InstanceIncarnation) (*instance.InstanceIncarnation, error) {
				return nil, nil
			},
		},
		{
			name: "VM lookup fails temporarily",
			incarnation: func(*instance.InstanceIncarnation) (*instance.InstanceIncarnation, error) {
				return nil, errors.New("compute unavailable")
			},
			wantRetry: true,
		},

		// Authorization and cluster policy, in the order they must be checked.
		{
			name:      "subject access review denies the requester",
			sarDenied: true,
			wantSARs:  1,
		},
		{
			name:      "subject access review fails temporarily",
			sarError:  true,
			wantRetry: true,
		},
		{
			name:           "Shielded Nodes enabled",
			policy:         []policyAnswer{{enabled: true}},
			wantSARs:       1,
			wantPolicyRead: 1,
		},
		{
			name:           "Shielded Nodes policy unknown",
			policy:         []policyAnswer{{err: errors.New("policy unavailable")}},
			wantRetry:      true,
			wantSARs:       1,
			wantPolicyRead: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := validRequest
			if tc.request != nil {
				request = tc.request(t)
			}
			csr := legacyCSR(request)
			if tc.csr != nil {
				tc.csr(csr)
			}
			claim := launchedLegacyClaim()
			if tc.claim != nil {
				tc.claim(&claim)
			}
			claims := []karpv1.NodeClaim{claim}
			if tc.claims != nil {
				claims = tc.claims(claim)
			}
			live, liveErr := liveIncarnation(), error(nil)
			if tc.incarnation != nil {
				live, liveErr = tc.incarnation(live)
			}
			instances := &legacyInstances{t: t, incarnation: live, err: liveErr}
			policy := &scriptedPolicy{answers: tc.policy}
			if tc.policy == nil {
				policy = disabledPolicy()
			}

			api, double := newKubeAPIDouble(t)
			double.sarAllowed = !tc.sarDenied
			double.sarFailure = tc.sarError
			controller := NewController(api, instances, policy)
			controller.client = &csrReadClient{request: csr, claims: claims}

			_, err := controller.Reconcile(context.Background())
			if tc.wantRetry {
				require.Error(t, err)
				require.True(t, isRetryable(err), "an unknown state must stay retryable, got %v", err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantPolicyRead, policy.reads, "the policy is read last, only for a request that passed every other check")
			approvals := double.recordedApprovals()
			reviews := double.recordedReviews()
			if !tc.wantApproved {
				require.Empty(t, approvals)
				require.Len(t, reviews, tc.wantSARs)
				return
			}
			require.Len(t, approvals, 1)
			require.Len(t, reviews, tc.wantSARs)
			// The original authenticated identity is authorized for nodeclient, unchanged.
			require.Equal(t, nodeClientSubresource, reviews[0].Spec.ResourceAttributes.Subresource)
			require.Equal(t, legacyBootstrapUsername, reviews[0].Spec.User)
			require.Equal(t, "8d3b1a4e-2222-4444-8888-000000000002", reviews[0].Spec.UID)
			require.Equal(t, []string{"system:authenticated"}, reviews[0].Spec.Groups)
			require.Equal(t, []string{"legacy-bootstrap"}, []string(reviews[0].Spec.Extra["authentication.kubernetes.io/credential-id"]))
		})
	}
}

func legacyOrganizations(t *testing.T, organizations ...string) []byte {
	t.Helper()
	return legacyRequestPEM(t, func(r *x509.CertificateRequest) { r.Subject.Organization = organizations })
}

// TestLegacyBootstrapReadsPolicyForEveryAttempt guards the downgrade window: a
// permissive answer is never reused by a later attempt or a second request.
func TestLegacyBootstrapReadsPolicyForEveryAttempt(t *testing.T) {
	policy := &scriptedPolicy{answers: []policyAnswer{{}, {enabled: true}, {err: errors.New("policy unavailable")}, {}}}
	instances := &legacyInstances{t: t, incarnation: liveIncarnation()}
	api, double := newKubeAPIDouble(t)
	double.sarAllowed = true
	controller := NewController(api, instances, policy)
	controller.client = &csrReadClient{
		request: legacyCSR(legacyRequestPEM(t, nil)),
		claims:  []karpv1.NodeClaim{launchedLegacyClaim()},
	}

	var approvedAfter []int
	for attempt := 1; attempt <= 4; attempt++ {
		_, _ = controller.Reconcile(context.Background())
		approvedAfter = append(approvedAfter, len(double.recordedApprovals()))
	}
	require.Equal(t, []int{1, 1, 1, 2}, approvedAfter, "only the attempts that read a disabled policy approve")
	require.Equal(t, 4, policy.reads)

	// Two requests in one reconcile each read the policy; the second sees enabled.
	policy = &scriptedPolicy{answers: []policyAnswer{{}, {enabled: true}}}
	double.mu.Lock()
	double.approvals = nil
	double.mu.Unlock()
	controller = NewController(api, instances, policy)
	first, second := legacyCSR(legacyRequestPEM(t, nil)), legacyCSR(legacyRequestPEM(t, nil))
	first.Name, second.Name = "csr-first", "csr-second"
	controller.client = &csrReadClient{
		requests: []*certv1.CertificateSigningRequest{first, second},
		claims:   []karpv1.NodeClaim{launchedLegacyClaim()},
	}
	_, err := controller.Reconcile(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, policy.reads)
	approvals := double.recordedApprovals()
	require.Len(t, approvals, 1)
	require.Equal(t, "csr-first", approvals[0].Name)
}

// TestLegacyFailureDoesNotBlockIndependentRequests keeps a failing legacy policy
// read from stopping an ordinary self-renewal, which needs no policy.
func TestLegacyFailureDoesNotBlockIndependentRequests(t *testing.T) {
	legacy := legacyCSR(legacyRequestPEM(t, nil))
	legacy.Name = "csr-legacy"
	nodeName := "karpenter-other"
	selfRenewal := nodeCSR(nodeRequest(t, nodeName), nodeName)
	selfRenewal.Name = "csr-renewal"

	policy := &scriptedPolicy{answers: []policyAnswer{{err: errors.New("policy unavailable")}}}
	api, double := newKubeAPIDouble(t)
	double.sarAllowed = true
	controller := NewController(api, &legacyInstances{t: t, incarnation: liveIncarnation()}, policy)
	controller.client = &csrReadClient{
		requests: []*certv1.CertificateSigningRequest{legacy, selfRenewal},
		claims:   []karpv1.NodeClaim{launchedLegacyClaim(), registeredNodeClaim("other", nodeName, 42)},
	}

	_, err := controller.Reconcile(context.Background())
	require.Error(t, err, "the unknown policy is reported for retry")
	require.Equal(t, 1, policy.reads, "self renewal must not read the policy")

	approvals := double.recordedApprovals()
	require.Len(t, approvals, 1)
	require.Equal(t, "csr-renewal", approvals[0].Name)
	reviews := double.recordedReviews()
	require.Len(t, reviews, 2)
	require.Equal(t, selfNodeClientSubresource, reviews[1].Spec.ResourceAttributes.Subresource)
}

// TestTPMBootstrapRequesterGetsNoLegacyFallback proves a missing proof is never
// treated as legacy: the TPM requesters stay rejected on a disabled cluster and
// the policy is not even consulted.
func TestTPMBootstrapRequesterGetsNoLegacyFallback(t *testing.T) {
	for _, requester := range []string{nodePoolBootstrapUsername, tpmBootstrapUsername} {
		t.Run(requester, func(t *testing.T) {
			csr := legacyCSR(legacyRequestPEM(t, nil))
			csr.Spec.Username = requester
			policy := disabledPolicy()
			api, double := newKubeAPIDouble(t)
			double.sarAllowed = true
			controller := NewController(api, &legacyInstances{t: t, incarnation: liveIncarnation()}, policy)
			controller.client = &csrReadClient{request: csr, claims: []karpv1.NodeClaim{launchedLegacyClaim()}}

			_, err := controller.Reconcile(context.Background())
			require.NoError(t, err)
			require.Empty(t, double.recordedApprovals())
			require.Empty(t, double.recordedReviews())
			require.Zero(t, policy.reads)
		})
	}
}
