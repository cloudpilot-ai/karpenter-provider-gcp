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
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	authorizationv1 "k8s.io/api/authorization/v1"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/instance"
)

const (
	nodeUserPrefix            = "system:node:"
	nodesGroup                = "system:nodes"
	nodePoolBootstrapUsername = "kubelet-nodepool-bootstrap"
	tpmBootstrapUsername      = "kubelet-bootstrap"
	// legacyBootstrapUsername is the requester GKE uses for an un-attested kubelet
	// bootstrap on a cluster with Shielded GKE Nodes disabled.
	legacyBootstrapUsername = "kubelet"
	nodePoolOrgPrefix       = "gke:nodepool:name:"
	instanceOrgPrefix       = "gke:instance:"

	// nodeClientSubresource is the authorization subresource checked for
	// bootstrap-authenticated enrollment. GKE's TPM bootstrap credential is
	// granted nodeclient, not selfnodeclient.
	nodeClientSubresource     = "nodeclient"
	selfNodeClientSubresource = "selfnodeclient"
)

var (
	requiredUsages = sets.New(certv1.UsageClientAuth)
	allowedUsages  = sets.New(certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageClientAuth)

	// legacyUsageSets are the only usage lists a legacy bootstrap request may
	// carry: the observed native pair, and Google's accepted pair plus key
	// encipherment.
	legacyUsageSets = []sets.Set[certv1.KeyUsage]{
		sets.New(certv1.UsageDigitalSignature, certv1.UsageClientAuth),
		sets.New(certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageClientAuth),
	}
)

type kubeletClientSubject struct {
	nodeName    string
	nodePoolOrg string
	instanceOrg string
}

// IdentityProvider resolves fresh Compute Engine evidence for a live VM.
// *instance.DefaultProvider implements it.
type IdentityProvider interface {
	// GetIdentity returns attestation identity evidence for TPM bootstrap.
	GetIdentity(ctx context.Context, zone, name string) (*instance.InstanceIdentity, error)
	// GetIncarnation returns the VM incarnation and ownership label for legacy
	// bootstrap, which has no attestation key.
	GetIncarnation(ctx context.Context, zone, name string) (*instance.InstanceIncarnation, error)
}

// ShieldedPolicyProvider reads the cluster's current Shielded GKE Nodes policy.
// *gke.DefaultProvider implements it. Implementations must not serve a cached
// answer and must return an error when the policy is unknown.
type ShieldedPolicyProvider interface {
	ShieldedNodesEnabled(ctx context.Context) (bool, error)
}

// Controller approves kubelet client CSRs for NodeClaims managed by this
// provider.
//
// Reference for CSR recognition, TPM verification and SAR authorization:
// https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gcp-controller-manager/node_csr_approver.go
// Copyright 2018 The Kubernetes Authors. Licensed under the Apache License, Version 2.0.
// Check relevant changes against this pinned implementation and keep intentional
// standalone-VM differences explicit.
//
// Differences from that implementation, required by standalone Karpenter VMs:
// cluster membership is the managed NodeClaim and its recorded VM incarnation
// instead of a cluster-managed instance group; the stable Compute
// getShieldedInstanceIdentity API supplies the attestation key; no serving
// certificate or stale-node path is implemented. Google's legacy un-attested
// bootstrap validator (requester "kubelet") is adapted, not copied: it is only
// reachable while a fresh read of the cluster's Shielded GKE Nodes policy says
// disabled, and it additionally requires a strict plain request, exactly one
// system:nodes organization, an exact usage list, a launched and not yet
// registered NodeClaim, and the live VM's recorded incarnation and NodeClaim UID
// label. Managed GKE's own internal enablement of that validator is not public.
type Controller struct {
	client           client.Client
	kubeClient       kubernetes.Interface
	identityProvider IdentityProvider
	shieldedPolicy   ShieldedPolicyProvider
}

func NewController(kubeClient kubernetes.Interface, identityProvider IdentityProvider, shieldedPolicy ShieldedPolicyProvider) *Controller {
	return &Controller{
		kubeClient:       kubeClient,
		identityProvider: identityProvider,
		shieldedPolicy:   shieldedPolicy,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	var csrList certv1.CertificateSigningRequestList
	if err := c.client.List(ctx, &csrList); err != nil {
		log.FromContext(ctx).Error(err, "unable to list CSRs")
		return reconciler.Result{}, err
	}

	var nodeClaimList karpv1.NodeClaimList
	if err := c.client.List(ctx, &nodeClaimList); err != nil {
		log.FromContext(ctx).Error(err, "unable to list NodeClaims")
		return reconciler.Result{}, err
	}

	// One request's temporary failure must not stop the others from progressing.
	var errs []error
	for i := range csrList.Items {
		csr := &csrList.Items[i]
		if isDecided(csr) {
			continue
		}

		log.FromContext(ctx).Info("reconciling CSR", "name", csr.Name)
		subresource, err := c.authorizeKubeletClientCSR(ctx, csr, nodeClaimList.Items)
		if err != nil {
			if isRetryable(err) {
				log.FromContext(ctx).Error(err, "unable to authorize CSR, retrying", "name", csr.Name, "username", csr.Spec.Username)
				errs = append(errs, fmt.Errorf("authorizing CSR %s: %w", csr.Name, err))
			} else {
				log.FromContext(ctx).V(1).Info("not approving CSR", "name", csr.Name, "username", csr.Spec.Username, "reason", err.Error())
			}
			continue
		}

		log.FromContext(ctx).Info("approving kubelet client CSR", "name", csr.Name, "username", csr.Spec.Username, "subresource", subresource)
		csr.Status.Conditions = append(csr.Status.Conditions, certv1.CertificateSigningRequestCondition{
			Type:           certv1.CertificateApproved,
			Status:         "True",
			Reason:         "KarpenterAutoApprover",
			Message:        "Automatically approved by Karpenter CSR controller",
			LastUpdateTime: metav1.Now(),
		})

		if _, err := c.kubeClient.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{}); err != nil {
			// The next reconcile re-reads the request, so a lost race with another
			// approver is reported and then treated as terminal.
			log.FromContext(ctx).Error(err, "failed to approve CSR", "name", csr.Name)
			errs = append(errs, fmt.Errorf("approving CSR %s: %w", csr.Name, err))
		}
	}
	return reconciler.Result{RequeueAfter: 30 * time.Second}, errors.Join(errs...)
}

// retryableError marks a temporary failure: the request stays pending and the
// reconciler retries it instead of treating it as ineligible.
type retryableError struct {
	err error
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	_, ok := errors.AsType[*retryableError](err)
	return ok
}

// authorizeKubeletClientCSR validates an undecided kubelet client CSR and
// returns the authorization subresource that must allow it. Any error means the
// request is not approved: temporary failures are wrapped in retryableError and
// everything else leaves the request for another approver.
func (c *Controller) authorizeKubeletClientCSR(ctx context.Context, csr *certv1.CertificateSigningRequest, nodeClaims []karpv1.NodeClaim) (string, error) {
	subject, request, groups, err := validateKubeletClientRequest(csr)
	if err != nil {
		return "", err
	}

	if csr.Spec.Username == nodeUserPrefix+subject.nodeName {
		if err := validateRenewal(subject, groups, nodeClaims); err != nil {
			return "", err
		}
		if err := c.authorizeCreate(ctx, csr, selfNodeClientSubresource); err != nil {
			return "", err
		}
		return selfNodeClientSubresource, nil
	}

	if csr.Spec.Username == legacyBootstrapUsername {
		if err := c.authorizeLegacyBootstrap(ctx, csr, request, subject, nodeClaims); err != nil {
			return "", err
		}
		return nodeClientSubresource, nil
	}

	if csr.Spec.Username != nodePoolBootstrapUsername && csr.Spec.Username != tpmBootstrapUsername {
		return "", fmt.Errorf("requester %q cannot request a certificate for node %q", csr.Spec.Username, subject.nodeName)
	}
	if err := c.validateAttestedBootstrap(ctx, csr, request, subject, nodeClaims); err != nil {
		return "", err
	}
	if err := c.authorizeCreate(ctx, csr, nodeClientSubresource); err != nil {
		return "", err
	}
	return nodeClientSubresource, nil
}

// validateKubeletClientRequest applies the request-level rules shared by both
// identities: signer, usages, PKCS#10 signature, subject restrictions and the
// requested node-pool group.
func validateKubeletClientRequest(csr *certv1.CertificateSigningRequest) (kubeletClientSubject, *x509.CertificateRequest, sets.Set[string], error) {
	if csr.Spec.SignerName != certv1.KubeAPIServerClientKubeletSignerName {
		return kubeletClientSubject{}, nil, nil, fmt.Errorf("signer %q is not %q", csr.Spec.SignerName, certv1.KubeAPIServerClientKubeletSignerName)
	}
	usages := sets.New(csr.Spec.Usages...)
	if !usages.IsSuperset(requiredUsages) || !allowedUsages.IsSuperset(usages) {
		return kubeletClientSubject{}, nil, nil, fmt.Errorf("usages %v are not valid for a kubelet client certificate", csr.Spec.Usages)
	}
	requestPEM := csr.Spec.Request
	if csr.Spec.Username == legacyBootstrapUsername {
		requestPEM = bytes.TrimSpace(requestPEM)
	}
	subject, request, err := parseKubeletClientRequest(requestPEM)
	if err != nil {
		return kubeletClientSubject{}, nil, nil, err
	}
	groups := sets.New(csr.Spec.Groups...)
	if subject.nodePoolOrg != "" && !groups.Has(subject.nodePoolOrg) {
		return kubeletClientSubject{}, nil, nil, fmt.Errorf("requester %q is not in group %q", csr.Spec.Username, subject.nodePoolOrg)
	}
	return subject, request, groups, nil
}

// validateAttestedBootstrap requires a valid TPM attestation proof for a
// bootstrap-authenticated request and binds it to the NodeClaim's recorded VM
// incarnation. There is no un-attested fallback for either bootstrap username.
func (c *Controller) validateAttestedBootstrap(ctx context.Context, csr *certv1.CertificateSigningRequest, request *x509.CertificateRequest, subject kubeletClientSubject, nodeClaims []karpv1.NodeClaim) error {
	nodeClaim := findBootstrapNodeClaim(subject, nodeClaims)
	if nodeClaim == nil {
		return fmt.Errorf("no NodeClaim was launched for node %q", subject.nodeName)
	}

	proof, err := parseAttestationEnvelope(csr.Spec.Request, request)
	if err != nil {
		return fmt.Errorf("parsing TPM attestation proof: %w", err)
	}

	recordedID, err := recordedIncarnation(nodeClaim)
	if err != nil {
		return err
	}

	project, zone, instanceName := parseProviderID(nodeClaim.Status.ProviderID)
	if project == "" || zone == "" || instanceName != subject.nodeName {
		return fmt.Errorf("NodeClaim %q has no complete GCE identity for node %q", nodeClaim.Name, subject.nodeName)
	}
	if err := validateProofIdentity(proof, project, zone, instanceName, recordedID); err != nil {
		return err
	}
	if err := validateBootstrapLifecycle(nodeClaim, subject); err != nil {
		return err
	}

	return c.verifyAttestedIdentity(ctx, nodeClaim, subject, proof, request, recordedID)
}

// authorizeLegacyBootstrap approves initial enrollment without TPM attestation
// for the legacy "kubelet" requester. This is a distinct, weaker path: possession
// of the shared bootstrap credential, not a TPM proof, is the only evidence that
// the requester controls the VM, so a holder could ask for another eligible
// pending NodeClaim. It is only reachable while the cluster's Shielded GKE Nodes
// policy is freshly read as disabled; the read is the last check before approval
// and is repeated for every attempt. A request for a TPM envelope, or without a
// proof where Shielded Nodes is enabled, is never approved here.
func (c *Controller) authorizeLegacyBootstrap(ctx context.Context, csr *certv1.CertificateSigningRequest, request *x509.CertificateRequest, subject kubeletClientSubject, nodeClaims []karpv1.NodeClaim) error {
	if err := validateLegacyRequest(csr, request); err != nil {
		return err
	}
	nodeClaim := findBootstrapNodeClaim(subject, nodeClaims)
	if nodeClaim == nil {
		return fmt.Errorf("no NodeClaim was launched for node %q", subject.nodeName)
	}
	if err := validateLegacyLifecycle(nodeClaim); err != nil {
		return err
	}
	if err := c.verifyLegacyIncarnation(ctx, nodeClaim); err != nil {
		return err
	}
	if err := c.authorizeCreate(ctx, csr, nodeClientSubresource); err != nil {
		return err
	}
	return c.requireShieldedNodesDisabled(ctx)
}

// validateLegacyRequest applies the strict legacy request shape on top of the
// shared checks: one plain certificate request and nothing else in the request
// field, subject O=system:nodes only, and an exact usage list.
func validateLegacyRequest(csr *certv1.CertificateSigningRequest, request *x509.CertificateRequest) error {
	usages := sets.New(csr.Spec.Usages...)
	if len(usages) != len(csr.Spec.Usages) || !slices.ContainsFunc(legacyUsageSets, usages.Equal) {
		return fmt.Errorf("usages %v are not valid for a legacy kubelet client certificate", csr.Spec.Usages)
	}
	// Subject.Names holds every subject attribute: only CN and the single organization.
	if !slices.Equal(request.Subject.Organization, []string{nodesGroup}) || len(request.Subject.Names) != 2 {
		return fmt.Errorf("legacy kubelet subject must be exactly CN and one organization %q", nodesGroup)
	}
	// pem.Decode skips leading malformed data and callers discard its remainder, so
	// the whole field is checked to hold one certificate request and nothing else.
	block, rest, err := decodeAttestationBlock(bytes.TrimSpace(csr.Spec.Request))
	if err != nil {
		return fmt.Errorf("legacy kubelet request must be a single plain certificate request: %w", err)
	}
	if block.Type != csrRequestBlock || len(block.Headers) > 0 || len(bytes.TrimSpace(rest)) > 0 || !bytes.Equal(block.Bytes, request.Raw) {
		return fmt.Errorf("legacy kubelet request must be a single plain certificate request")
	}
	return nil
}

// validateLegacyLifecycle restricts legacy enrollment to the initial credential of
// a launched claim. Unlike attested re-enrollment it never serves a registered or
// draining claim.
func validateLegacyLifecycle(nodeClaim *karpv1.NodeClaim) error {
	if !nodeClaim.DeletionTimestamp.IsZero() || !nodeClaim.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() {
		return fmt.Errorf("NodeClaim %q is deleting or has not launched", nodeClaim.Name)
	}
	if nodeClaim.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() || nodeClaim.Status.NodeName != "" {
		return fmt.Errorf("NodeClaim %q is already registered", nodeClaim.Name)
	}
	return nil
}

// verifyLegacyIncarnation requires the live VM to be the incarnation the NodeClaim
// recorded at launch and to carry that NodeClaim's UID. Nothing is backfilled or
// trusted by name alone.
func (c *Controller) verifyLegacyIncarnation(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	recordedID, err := recordedIncarnation(nodeClaim)
	if err != nil {
		return err
	}
	project, zone, instanceName := parseProviderID(nodeClaim.Status.ProviderID)
	if project == "" || zone == "" || instanceName == "" {
		return fmt.Errorf("NodeClaim %q has no complete GCE identity", nodeClaim.Name)
	}
	live, err := c.identityProvider.GetIncarnation(ctx, zone, instanceName)
	if err != nil {
		return &retryableError{fmt.Errorf("reading the incarnation of instance %q: %w", instanceName, err)}
	}
	if live == nil {
		return fmt.Errorf("instance %q for NodeClaim %q does not exist", instanceName, nodeClaim.Name)
	}
	return validateLegacyIncarnation(live, nodeClaim, project, recordedID)
}

// validateLegacyIncarnation compares the live VM with the NodeClaim's recorded
// launch instance and ownership. The expected values come from the NodeClaim.
func validateLegacyIncarnation(live *instance.InstanceIncarnation, nodeClaim *karpv1.NodeClaim, project string, recordedID uint64) error {
	_, zone, instanceName := parseProviderID(nodeClaim.Status.ProviderID)
	if live.ProjectID != project || live.Zone != zone || live.Name != instanceName || live.NumericID != recordedID {
		return fmt.Errorf("live instance %q does not match the recorded launch instance %d of NodeClaim %q", instanceName, recordedID, nodeClaim.Name)
	}
	if nodeClaim.UID == "" || live.NodeClaimUID != string(nodeClaim.UID) {
		return fmt.Errorf("live instance %q is not labeled as owned by NodeClaim %q", instanceName, nodeClaim.Name)
	}
	return nil
}

// requireShieldedNodesDisabled reads the cluster policy fresh. Only a known
// disabled policy allows legacy approval; an unknown policy stays retryable.
func (c *Controller) requireShieldedNodesDisabled(ctx context.Context) error {
	enabled, err := c.shieldedPolicy.ShieldedNodesEnabled(ctx)
	if err != nil {
		return &retryableError{fmt.Errorf("reading the cluster Shielded Nodes policy: %w", err)}
	}
	if enabled {
		return fmt.Errorf("cluster has Shielded GKE Nodes enabled; un-attested bootstrap is not allowed")
	}
	return nil
}

// verifyAttestedIdentity completes the attested checks that need the live VM:
// the requested instance organization, the authoritative identity and the TPM
// proof itself.
func (c *Controller) verifyAttestedIdentity(ctx context.Context, nodeClaim *karpv1.NodeClaim, subject kubeletClientSubject, proof attestationProof, request *x509.CertificateRequest, recordedID uint64) error {
	_, _, instanceName := parseProviderID(nodeClaim.Status.ProviderID)
	identity, err := c.lookupAttestationIdentity(ctx, nodeClaim, proof, recordedID)
	if err != nil {
		return err
	}
	// Bootstrap credentials are not authenticated into the requested instance
	// group, so the whole organization has to be authorized from the verified VM.
	if err := validateInstanceOrganization(subject, identity.ProjectNumber, identity.Zone, identity.NumericID); err != nil {
		return err
	}
	attestationKey, err := parseAttestationKey(identity.AttestationKeyPEM)
	if err != nil {
		return fmt.Errorf("reading the attestation key of instance %q: %w", instanceName, err)
	}
	return verifyAttestationProof(proof, request, attestationKey)
}

// validateInstanceOrganization binds a requested gke:instance organization to the
// verified VM incarnation. The organization names a project number, zone and
// instance ID that the approved certificate carries into the node's authenticated
// groups, so all three must match authoritative identity rather than only the zone.
func validateInstanceOrganization(subject kubeletClientSubject, projectNumber uint64, zone string, numericID uint64) error {
	if subject.instanceOrg == "" {
		return nil
	}
	orgProject, orgZone, orgID, err := parseInstanceOrg(subject.instanceOrg)
	if err != nil {
		return err
	}
	if orgProject != projectNumber || orgZone != zone || orgID != numericID {
		return fmt.Errorf("subject organization %q does not match the verified instance %d/%s/%d", subject.instanceOrg, projectNumber, zone, numericID)
	}
	return nil
}

// validateProofIdentity rejects a proof for another project, zone, VM name or
// VM incarnation. The expected values never come from the proof itself.
func validateProofIdentity(proof attestationProof, project, zone, instanceName string, recordedID uint64) error {
	if proof.vm.Name != instanceName || proof.vm.Zone != zone {
		return fmt.Errorf("VM IDENTITY %s/%s does not match the requested instance %s/%s", proof.vm.ProjectName, proof.vm.Zone, project, zone)
	}
	if proof.vm.ProjectName != project {
		return fmt.Errorf("VM IDENTITY project %q does not match the configured project %q", proof.vm.ProjectName, project)
	}
	if proof.vm.ID != recordedID {
		return fmt.Errorf("VM IDENTITY instance %d does not match the recorded launch instance %d of NodeClaim %q", proof.vm.ID, recordedID, instanceName)
	}
	return nil
}

// lookupAttestationIdentity reads the live VM identity and attestation evidence
// and requires them to describe the incarnation the NodeClaim recorded.
func (c *Controller) lookupAttestationIdentity(ctx context.Context, nodeClaim *karpv1.NodeClaim, proof attestationProof, recordedID uint64) (*instance.InstanceIdentity, error) {
	_, zone, instanceName := parseProviderID(nodeClaim.Status.ProviderID)
	identity, err := c.identityProvider.GetIdentity(ctx, zone, instanceName)
	if err != nil {
		return nil, &retryableError{fmt.Errorf("reading the identity of instance %q: %w", instanceName, err)}
	}
	if identity == nil {
		return nil, fmt.Errorf("instance %q for NodeClaim %q does not exist", instanceName, nodeClaim.Name)
	}
	if identity.ProjectID != proof.vm.ProjectName || identity.ProjectNumber != proof.vm.ProjectID {
		return nil, fmt.Errorf("VM IDENTITY project does not match the configured cluster project")
	}
	if identity.NumericID != recordedID || identity.Zone != zone || identity.Name != instanceName {
		return nil, fmt.Errorf("live instance %q identity does not match the recorded launch instance %d of NodeClaim %q", instanceName, recordedID, nodeClaim.Name)
	}
	return identity, nil
}

// findBootstrapNodeClaim returns the managed NodeClaim launched for the requested
// node name. Bootstrap-authenticated requests never select a claim by any other
// property supplied by the requester.
func findBootstrapNodeClaim(subject kubeletClientSubject, nodeClaims []karpv1.NodeClaim) *karpv1.NodeClaim {
	for i := range nodeClaims {
		nc := &nodeClaims[i]
		_, _, instanceName := parseProviderID(nc.Status.ProviderID)
		if isManagedNodeClaim(nc) && instanceName != "" && instanceName == subject.nodeName {
			return nc
		}
	}
	return nil
}

// recordedIncarnation returns the numeric VM ID recorded for the NodeClaim when
// it was launched or adopted.
func recordedIncarnation(nodeClaim *karpv1.NodeClaim) (uint64, error) {
	recorded := nodeClaim.Annotations[v1alpha1.AnnotationInstanceNumericID]
	if recorded == "" {
		return 0, fmt.Errorf("NodeClaim %q has no recorded VM instance ID; only claims launched by this controller can use attested enrollment", nodeClaim.Name)
	}
	id, err := strconv.ParseUint(recorded, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("NodeClaim %q has an invalid recorded VM instance ID %q", nodeClaim.Name, recorded)
	}
	return id, nil
}

// authorizeCreate asks the API server whether the request's original
// authenticated identity may create the CSR subresource that path requires.
func (c *Controller) authorizeCreate(ctx context.Context, csr *certv1.CertificateSigningRequest, subresource string) error {
	extra := make(map[string]authorizationv1.ExtraValue, len(csr.Spec.Extra))
	for k, v := range csr.Spec.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   csr.Spec.Username,
			UID:    csr.Spec.UID,
			Groups: csr.Spec.Groups,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group:       "certificates.k8s.io",
				Resource:    "certificatesigningrequests",
				Verb:        "create",
				Subresource: subresource,
			},
		},
	}
	result, err := c.kubeClient.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return &retryableError{fmt.Errorf("creating subject access review for %s: %w", subresource, err)}
	}
	if !result.Status.Allowed {
		return fmt.Errorf("requester %q is not allowed to create certificatesigningrequests/%s", csr.Spec.Username, subresource)
	}
	return nil
}

func validateRenewal(subject kubeletClientSubject, groups sets.Set[string], nodeClaims []karpv1.NodeClaim) error {
	if !groups.Has(nodesGroup) {
		return fmt.Errorf("requester is not in group %q", nodesGroup)
	}
	if subject.instanceOrg != "" && !groups.Has(subject.instanceOrg) {
		return fmt.Errorf("requester is not in group %q", subject.instanceOrg)
	}
	for i := range nodeClaims {
		nc := &nodeClaims[i]
		_, _, instanceName := parseProviderID(nc.Status.ProviderID)
		if isManagedNodeClaim(nc) && nc.Status.NodeName == subject.nodeName && instanceName == subject.nodeName && nc.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
			return nil
		}
	}
	return fmt.Errorf("no NodeClaim is registered as node %q", subject.nodeName)
}

// validateBootstrapLifecycle restricts an attested bootstrap request either to
// initial enrollment of a launched claim or to re-enrollment of the same live,
// registered claim, which stays possible while the node drains.
func validateBootstrapLifecycle(nodeClaim *karpv1.NodeClaim, subject kubeletClientSubject) error {
	if nodeClaim.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
		if nodeClaim.Status.NodeName != "" && nodeClaim.Status.NodeName != subject.nodeName {
			return fmt.Errorf("NodeClaim %q is registered as node %q", nodeClaim.Name, nodeClaim.Status.NodeName)
		}
		return nil
	}
	if !nodeClaim.DeletionTimestamp.IsZero() || !nodeClaim.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() {
		return fmt.Errorf("NodeClaim %q is deleting or has not launched", nodeClaim.Name)
	}
	if nodeClaim.Status.NodeName != "" {
		return fmt.Errorf("NodeClaim %q for node %q is already registered", nodeClaim.Name, subject.nodeName)
	}
	return nil
}

func isManagedNodeClaim(nc *karpv1.NodeClaim) bool {
	ref := nc.Spec.NodeClassRef
	return ref != nil && ref.Group == apis.Group && ref.Kind == "GCENodeClass"
}

// parseKubeletClientRequest parses and validates the CSR that must be the first
// PEM block of the request, and returns the parsed request for callers that need
// its public key.
func parseKubeletClientRequest(request []byte) (kubeletClientSubject, *x509.CertificateRequest, error) {
	block, _ := pem.Decode(request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return kubeletClientSubject{}, nil, fmt.Errorf("request is not a PEM-encoded certificate request")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return kubeletClientSubject{}, nil, fmt.Errorf("parsing certificate request: %w", err)
	}
	if err := req.CheckSignature(); err != nil {
		return kubeletClientSubject{}, nil, fmt.Errorf("checking certificate request signature: %w", err)
	}
	if len(req.DNSNames) > 0 || len(req.EmailAddresses) > 0 || len(req.IPAddresses) > 0 || len(req.URIs) > 0 {
		return kubeletClientSubject{}, nil, fmt.Errorf("kubelet client certificate requests cannot contain subject alternative names")
	}
	subject, err := parseKubeletClientSubject(req.Subject.CommonName, req.Subject.Organization)
	if err != nil {
		return kubeletClientSubject{}, nil, err
	}
	return subject, req, nil
}

func parseKubeletClientSubject(commonName string, organizations []string) (kubeletClientSubject, error) {
	var subject kubeletClientSubject
	var ok bool
	subject.nodeName, ok = strings.CutPrefix(commonName, nodeUserPrefix)
	if !ok || subject.nodeName == "" {
		return kubeletClientSubject{}, fmt.Errorf("subject common name %q does not start with %q", commonName, nodeUserPrefix)
	}
	if !slices.Contains(organizations, nodesGroup) {
		return kubeletClientSubject{}, fmt.Errorf("subject organization %v does not include %s", organizations, nodesGroup)
	}
	for _, o := range organizations {
		var field *string
		switch {
		case o == nodesGroup:
			continue
		case strings.HasPrefix(o, nodePoolOrgPrefix):
			field = &subject.nodePoolOrg
		case strings.HasPrefix(o, instanceOrgPrefix):
			field = &subject.instanceOrg
		default:
			return kubeletClientSubject{}, fmt.Errorf("subject organization %q is not allowed", o)
		}
		if *field != "" {
			return kubeletClientSubject{}, fmt.Errorf("subject organization %v repeats %q", organizations, o)
		}
		*field = o
	}
	return subject, nil
}

// parseInstanceOrg parses O=gke:instance:<project-number>:<zone>:<instance-id>.
func parseInstanceOrg(org string) (projectNumber uint64, zone string, numericID uint64, err error) {
	parts := strings.Split(strings.TrimPrefix(org, instanceOrgPrefix), ":")
	if len(parts) != 3 {
		return 0, "", 0, fmt.Errorf("subject organization %q is not a complete GCE instance identity", org)
	}
	projectNumber, projectErr := strconv.ParseUint(parts[0], 10, 64)
	numericID, idErr := strconv.ParseUint(parts[2], 10, 64)
	if projectErr != nil || idErr != nil || projectNumber == 0 || numericID == 0 || parts[1] == "" {
		return 0, "", 0, fmt.Errorf("subject organization %q is not a complete GCE instance identity", org)
	}
	return projectNumber, parts[1], numericID, nil
}

func parseProviderID(providerID string) (project, zone, instance string) {
	rest, ok := strings.CutPrefix(providerID, "gce://")
	if !ok {
		return "", "", ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

// isDecided reports whether a request already has a decision or an issued
// certificate. An issued certificate is never re-approved even without a
// terminal condition, matching the GKE approver's behavior.
func isDecided(csr *certv1.CertificateSigningRequest) bool {
	if len(csr.Status.Certificate) != 0 {
		return true
	}
	for _, cond := range csr.Status.Conditions {
		if cond.Type == certv1.CertificateApproved || cond.Type == certv1.CertificateDenied {
			return true
		}
	}
	return false
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	c.client = m.GetClient()
	return controllerruntime.NewControllerManagedBy(m).
		Named("csr-controller").
		For(&certv1.CertificateSigningRequest{}).
		Complete(singleton.AsReconciler(c))
}
