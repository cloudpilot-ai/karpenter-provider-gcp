# CSR approval security contract

Karpenter approves kubelet-client certificate signing requests (CSRs) for its GCE nodes. Approval is an authorization decision: matching a signer or certificate subject does not establish that the requester may obtain that identity.

## Approval policy

The controller validates the CSR signature, kubelet-client signer, allowed usages and subject. It rejects subject alternative names and organizations outside `system:nodes` and the supported GKE node-pool/instance organizations. Requested GKE node-pool groups must already belong to the API-authenticated requester; renewal instance groups must also already belong to that requester.

Eligible NodeClaims must reference `karpenter.k8s.gcp/GCENodeClass` and have a complete `gce://<project>/<zone>/<instance>` ProviderID matching the requested node name.

Three paths exist, and all require a successful `SubjectAccessReview` (SAR) for the original CSR username, UID, groups and extras:

- **Attested bootstrap:** the requester is `kubelet-nodepool-bootstrap` or `kubelet-bootstrap`. The request must carry the native GKE TPM attestation envelope and pass the checks below, then the requester must be allowed to `create certificatesigningrequests/nodeclient`.
- **Legacy un-attested bootstrap:** the requester is exactly `kubelet`, the identity GKE uses to enroll nodes in a cluster with Shielded GKE Nodes **disabled**. It is a distinct, weaker path described [below](#legacy-un-attested-bootstrap), and the requester must be allowed to `create certificatesigningrequests/nodeclient`.
- **Authenticated self-renewal:** the requester is `system:node:<requested-name>` and belongs to the authenticated `system:nodes` group. The NodeClaim has `Registered=True`, and its node name and GCE instance name match the request. Self-renewal remains allowed while that registered claim is draining/deleting, and the requester must be allowed to `create certificatesigningrequests/selfnodeclient`.

### Attested bootstrap verification

Both bootstrap usernames always require a valid proof. A missing or invalid proof is never approved through an un-attested fallback; the request simply stays pending. The four proof blocks are required; the native producer's optional `ATTESTATION CERTIFICATE` is accepted but never used as a trust anchor. The attestation key always comes from Compute, not the request.

1. The controller resolves the managed NodeClaim by the requested node name and requires its recorded numeric VM instance ID (annotation `karpenter.k8s.gcp/instance-numeric-id`, written by this provider at launch or adoption).
2. A requested `gke:instance:<project-number>:<zone>:<instance-id>` organization must match the verified project number, zone and numeric instance ID in full. Bootstrap credentials are not authenticated into that instance group, so approving a partially matching organization would mint another VM's authenticated group; an incomplete organization is rejected.
3. The `VM IDENTITY` block must name that NodeClaim's project, zone and VM name and report the same numeric instance ID. The proof controls no lookup target: project, zone and name always come from the NodeClaim and the configured provider settings.
4. The controller reads the live VM fresh from the Compute API (not the name-keyed cache), addresses the vTPM attestation-key read by that numeric incarnation, and repeats the VM read afterwards, rejecting a VM replaced during the lookup. The live numeric instance ID, the configured project's authoritative numeric ID and the attestation key must agree with the proof.
5. The `ATTESTATION SIGNATURE` must verify with that key over `ATTESTATION DATA`, the data must decode to a TPM certify structure with no non-canonical or trailing bytes, and the certified TPM object must match the CSR public key.
6. The NodeClaim lifecycle must allow the request: either initial enrollment (`Launched=True`, not registered, not deleting) or re-enrollment of the same live registered claim, which includes reissuance while it drains.

### Legacy un-attested bootstrap

The `kubelet` requester carries no TPM proof. It is approved only while the cluster's Shielded GKE Nodes setting is read as disabled, and only for initial enrollment of one pending NodeClaim. It is never a fallback: a missing, malformed or invalid proof from `kubelet-nodepool-bootstrap` or `kubelet-bootstrap` stays pending whatever the cluster setting, and neither the setting nor the legacy path is consulted for attested bootstrap or self-renewal.

The request must satisfy a stricter shape than the attested path:

- `request` holds exactly one plain `CERTIFICATE REQUEST` PEM block. Only whitespace may surround it; leading or trailing text, a malformed leading PEM block, a duplicate or any extra block (including a TPM envelope) is rejected.
- The subject is `CN=system:node:<name>` with exactly one organization, `system:nodes`: no node-pool, instance or other organization and no other subject attribute.
- The usages are exactly `digital signature` and `client auth`, optionally with `key encipherment` (the pair Google's legacy validator accepts); no other subset and no duplicates.
- The signer, signature and no-SAN checks are shared with the other paths.

The NodeClaim must be launched (`Launched=True`), not registered (`Registered` not true, no node name), not deleting, and record a valid non-zero `karpenter.k8s.gcp/instance-numeric-id`. A registered or draining claim never uses this path. The controller reads the live VM fresh from Compute (no instance cache, no attestation key) and requires its project, zone, name and numeric instance ID to match the claim's ProviderID and recorded incarnation, and its NodeClaim-UID ownership label to name this NodeClaim. An unlabelled VM is not adopted or backfilled.

The setting is read from the GKE API with an uncached request after the checks above and the SAR, immediately before approval, and again on every attempt. An enabled setting means no approval; an absent, unreadable or failed answer is unknown and leaves the request pending for retry. A response with `shieldedNodes: {}` is a present, disabled setting (the API omits the proto3 default `false`); a missing `shieldedNodes` message is unknown, not disabled. There is no transaction across GKE, Compute and Kubernetes: this removes the application-cache window but not every race, and NodeClaim lifecycle comes from the reconcile's snapshot.

**This path is weaker than attested bootstrap.** The VM association is not a cryptographic proof that the requester is that VM. The legacy credential is shared by the cluster's bootstrapping nodes, so a holder can request a certificate for another eligible pending NodeClaim. It is the consequence of an operator disabling Shielded GKE Nodes, not an equivalent to TPM attestation. Keep Shielded GKE Nodes enabled to require mandatory proof.

The behavior follows a native enrollment observed on GKE 1.36.4-gke.2046003 (COS, one architecture) with Shielded GKE Nodes disabled: requester `kubelet`, signer `kubernetes.io/kube-apiserver-client-kubelet`, `digital signature` and `client auth`, one `system:nodes` organization, ECDSA P-256, no SANs, one plain PEM block. That is one profile, not a guarantee for every version, image or architecture. The SAR for this requester and the candidate controller's own legacy approval have not been validated against real GKE.

### Re-enrollment and rotation

The historical GKE exec auth plugin reissues a registered node's certificate through the same bootstrap-authenticated, attested path; that is not `system:node` self-renewal. The attested path therefore remains available for a registered claim as long as its recorded VM incarnation still exists, so a plugin-driven reissue during a drain is not blocked by the unregistered-only predicate that applies to initial enrollment. It is available only for claims this version launched or adopted with a recorded incarnation; a claim created by an earlier version has no attested re-enrollment path and needs replacement.

Both paths remain available for a registered claim during drain. Ordinary self-renewal is continuity of an already-issued identity, not fresh vTPM attestation, and this change does not revoke certificates that were already issued.

### Non-goals

The controller does not approve kubelet serving certificates, does not delete stale Nodes or Pods, does not provide a global switch to skip attestation, and does not implement a custom bootstrap protocol or nonce. Un-attested approval is limited to the cluster-controlled legacy path above.

## Trust and replay limits

The proof certifies the CSR public key with the VM attestation key. It does not sign a controller-issued nonce, the CSR UID, the NodeClaim UID or a cluster audience, and the kubelet TLS key is software-generated rather than guaranteed non-exportable. Recorded-incarnation, lifecycle and NodeClaim checks narrow replay and same-name replacement, but they are not one-time attestation or post-compromise protection: resubmitting the same proof for the same key and a still-eligible incarnation is not detected, and a stolen issued certificate may still renew through the self-renewal path.

Trusted controller configuration, privileged NodeClaim/annotation/status writers and VM-provisioning IAM are assumptions of the VM-to-NodeClaim association. A principal that can rewrite those trust anchors is outside the protection this verifier offers. The NodeClaim-UID label on the VM (`karpenter-k8s-gcp-nodeclaim-uid`) is an ownership association under that trusted IAM, not attestation evidence.

The legacy path has no such binding at all: it relies on the checks above and on the operator's disabled Shielded GKE Nodes setting.

Other approvers operate independently. These controller checks are not a claim about every cluster approval path, and a competing terminal denial is not overwritten.

## Regression tests

Run the focused offline authorization tests:

```sh
go test -race ./pkg/controllers/csr
```

They generate TPM attestation proofs and legacy requests locally and cover approval decisions, the SAR query, the legacy request shape, lifecycle, live incarnation, per-attempt policy reads, the approval API write and request independence using local HTTP doubles. `go test ./pkg/providers/gke ./pkg/providers/instance` covers the uncached policy read and the fresh incarnation lookup. They do not exercise real API admission, CA signing or GKE joining/rotation. [Proposal 0012](../../proposals/0012-gce-instance-bound-csr-approval.md) records the supported matrix and the pending real-GKE reissuance/drain validation.

## Chart certificate permissions

The controller's chart grants:

- `get`, `list`, `watch` on CSRs;
- `update` on the approval subresource;
- `approve` only for `kubernetes.io/kube-apiserver-client-kubelet`;
- `create` on `subjectaccessreviews`, used to query the API server with the original requester identity.

The controller's GCP service account additionally needs `compute.instances.getShieldedInstanceIdentity` and `compute.projects.get` to read the VM attestation key and the authoritative numeric project ID ([`deploy/iam/karpenter-controller-role.yaml`](../../deploy/iam/karpenter-controller-role.yaml)). The legacy path reads the cluster's Shielded GKE Nodes setting with `container.clusters.get`, which the role already grants.

These permissions are bound to the controller ServiceAccount. External cluster roles, group memberships, bindings and aggregation inputs require a separate authorized exposure audit. Existing gosec/CodeQL checks and Greptile review guidance are complementary, not proofs of correct authorization.
