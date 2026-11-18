# Proposal: GCE instance-bound kubelet CSR approval

- **Status**: Implementable (implementation under review; real-GKE reissuance/drain validation pending)
- **Authors**: @dm3ch
- **Created**: 2026-11-18
- **Related Issues**: [#778](https://github.com/karpenter-gcp/karpenter-provider-gcp/issues/778)

## Summary

Karpenter approves kubelet client CSRs only when the bootstrap requester proves possession of the GCE VM it claims. The proof is the native GKE TPM attestation envelope, verified against the VM attestation key that Compute reports for the live instance, and bound to the numeric VM incarnation recorded on the NodeClaim at launch or adoption.

This follows Google's [node CSR approver](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gcp-controller-manager/node_csr_approver.go) for proof parsing, signature verification, key binding, TPM structure validation and `SubjectAccessReview`, and replaces its managed-instance-group membership check with the authoritative NodeClaim/VM/cluster association available to a standalone-VM provider.

## Motivation

### Problem statement

Every GKE node pool has a bootstrap credential that authenticates as `kubelet-nodepool-bootstrap` or `kubelet-bootstrap`. Before this change the controller approved any such request for a launched, unregistered Karpenter NodeClaim, so a holder of another pool's credential could request a pending Karpenter node's identity. Name, zone and lifecycle checks narrowed that exposure but did not establish that the requester was the VM.

Issue #778 asks whether instance, cluster and CSR-key association can be established throughout enrollment, renewal and drain. A signature can authenticate a VM/key relationship, while configured-cluster membership and NodeClaim ownership remain authorization policy.

### Goals

- Reuse the existing GKE TPM proof format and upstream verification semantics.
- Bind approval to the live VM incarnation and the provider-owned NodeClaim in the configured cluster.
- Preserve certificate reissuance for a registered node, including while it drains, on both the attested bootstrap path and ordinary `system:node` self-renewal.
- Document prerequisites, replay limits and the evidence that remains outstanding.

### Non-goals

- A custom bootstrap agent, nonce protocol, metadata-token transport or managed-control-plane change.
- Serving-certificate approval, stale-Node/Pod deletion or migration to managed instance groups.
- Claiming non-exportable kubelet keys, revoking existing certificates or securing other approvers.

## Proposal

### Feasibility evidence

The public [Shielded GKE Nodes contract](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/shielded-gke-nodes#about_shielded_gke_nodes) requires a node to belong to a cluster-provisioned managed instance group (MIG). Karpenter provisions standalone VMs, so unchanged native managed-node approval cannot apply.

Bounded research in an existing Shielded GKE **1.35.8-gke.1796000** environment (COS/amd64, `n2-standard-2`, vTPM) observed the initial enrollment against the then-deployed **v0.8.2** controller. The corrected rotation probe ran against the existing **PR #793** deployment (source `3011887c`) and observed:

- Initial enrollment authenticated as `kubelet-nodepool-bootstrap` with an authenticated node-pool group, subject organizations `system:nodes`, a GKE node-pool organization and a GKE instance organization, and the four-block envelope (`CERTIFICATE REQUEST`, `ATTESTATION DATA`, `ATTESTATION SIGNATURE`, `VM IDENTITY`). The request was approved by `KarpenterAutoApprover` and the node became Ready.
- An age-triggered native plugin cache rotation of the same registered node requested a **new** key and submitted a second attested request with the same four blocks, the same bootstrap requester class, and the same subject identity. It remained `Pending`; no competing denial was recorded.
- Both real requests were validated in memory against the authoritative Compute attestation key: PKCS#10 signature, numeric VM identity, RSA attestation signature and TPM certified-public-key binding. Equivalent checks are reproduced by the local regression fixtures.
- A `SubjectAccessReview` for the bootstrap username and its authenticated groups for `create certificatesigningrequests/nodeclient` returned `Allowed=true` with no evaluation error, matching what the controller now performs.

Ubuntu, other GKE versions and arm64 were not evaluated, and no active kubelet certificate adoption, health or drain behavior was validated. The observed rotation branch establishes protocol feasibility only: neither the native protocol nor the controller's own enforcement was validated on a candidate deployment. Temporary research resources were removed and their absence checked; no further live authority was used.

### Implemented verification flow

`pkg/controllers/csr` splits the two supported identities:

1. **Attested bootstrap** (`kubelet-nodepool-bootstrap`, `kubelet-bootstrap`) always requires the four proof blocks. The producer's optional `ATTESTATION CERTIFICATE` is accepted once and ignored, matching the upstream API-key verification path: it cannot select or replace the authoritative Compute attestation key. Missing or invalid proof leaves the request pending; there is no un-attested fallback.
   - The managed NodeClaim is selected by the requested node name only.
   - The NodeClaim must carry the `karpenter.k8s.gcp/instance-numeric-id` annotation recorded at launch/adoption.
   - `VM IDENTITY` must match the NodeClaim's project, zone and VM name and the recorded numeric ID. The proof never selects the lookup target.
   - A requested `O=gke:instance:<project-number>:<zone>:<instance-id>` organization must match the authoritative project number, zone and numeric instance ID in full. Bootstrap credentials are not authenticated into that group, so a partially matching organization would otherwise let the approved certificate mint another VM's group.
   - `pkg/providers/instance` reads the VM and its RSA vTPM attestation key from the stable Compute `getShieldedInstanceIdentity` API, fresh rather than from the name-keyed cache, and re-reads the VM to reject a replacement during the lookup. `pkg/providers/gke` supplies the configured project's authoritative numeric ID through `Projects.Get`.
   - The attestation signature must verify with that key, the certification structure must decode to a TPM certify structure with no trailing or non-canonical bytes, and the certified object must match the CSR public key using the maintained `github.com/google/go-tpm` legacy TPM 2.0 parser.
   - Lifecycle: initial enrollment requires `Launched=True`, no registration and no deletion; re-enrollment requires the same live registered claim and remains allowed while it drains.
   - The original CSR username, UID, groups and extras are sent in a SAR for `create certificatesigningrequests/nodeclient`.
2. **Authenticated self-renewal** (`system:node:<same-name>`) keeps the existing registered-claim and group checks, requires no attestation, remains allowed during drain, and adds a SAR for `create certificatesigningrequests/selfnodeclient`.

Temporary Compute or SAR failures never approve or deny a request: they are reported for retry, and other requests in the same reconcile continue. Terminal conditions and approval conflicts are handled idempotently by re-listing on the next reconcile.

### Durable incarnation association

`InstanceID` and the GCE ProviderID remain the VM **name**. `pkg/providers/instance` now carries `NumericID` from the completed insert operation's `TargetId` (or, when both the insert response and the completed operation omit it, from an authoritative read that must show this NodeClaim's ownership label) and from adoption/`Get`/`List`. `Create` neither launches without a server-reported ID nor returns a VM whose incarnation it cannot vouch for: a legacy name-only adoption fails with an actionable `InstanceIncarnationUnknown` error instead of producing a NodeClaim that core marks launched. `Get`, `List` and adoption keep reporting those VMs so they can be cleaned up and keep renewing.

`pkg/cloudprovider` records it as the `karpenter.k8s.gcp/instance-numeric-id` annotation, which core's launch path merges into the NodeClaim, and refuses to replace an already-recorded ID with a different one. Launched VMs also carry a `karpenter-k8s-gcp-nodeclaim-uid` label; adoption rejects a same-named VM owned by another NodeClaim, and only records an incarnation when that ownership label matches. VMs created before the label existed remain adoptable by name but record no incarnation, so attested enrollment is unavailable for them until they are replaced.

Trusted controller configuration, privileged NodeClaim/annotation/status writers and VM-provisioning IAM are assumptions of this association. The label is an ownership association under that IAM, not attestation evidence.

### Lifecycle policy

| Path                        | Required association                                                                         | Authorization/lifecycle                                                                                            |
|-----------------------------|----------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------|
| Initial attested enrollment | Valid VM/key proof, recorded live incarnation, matching Compute identity and attestation key | Bootstrap principal, allowed `nodeclient` SAR; launched, unregistered, non-deleting NodeClaim.                     |
| Attested re-enrollment      | Fresh proof for the same live registered incarnation                                         | Bootstrap principal, allowed `nodeclient` SAR; allowed while the registered claim drains or is deleting.           |
| Ordinary self-renewal       | API-authenticated `system:node:<same-name>` and registered-claim/group restrictions          | Allowed `selfnodeclient` SAR; preserved during drain. No attestation envelope is required on this continuity path. |

### Replay and trust limits

The proof certifies the CSR public key with the VM attestation key. It does **not** sign a controller-issued nonce, CSR UID, NodeClaim UID or cluster audience. Recorded-incarnation, lifecycle and NodeClaim checks authorize the proof in the current cluster and reject different-key, cross-instance and same-name replacement substitution; they do not turn unsigned context into a cryptographic binding. Resubmitting a proof for the same key and a still-eligible incarnation is not detected, the software-generated kubelet key is not guaranteed non-exportable, and a stolen issued certificate may still renew through the self-renewal path. Existing certificates are not revoked; deleting a CSR does not revoke one.

Other approvers operate independently, and a competing terminal denial is not overwritten.

## Prerequisites and migration

- Evidence so far: the native proof format, native cache-rotation branch and the required bootstrap `nodeclient` authorization were observed in plaintext/read-only probes on Shielded GKE **1.35.8-gke.1796000** with COS/amd64 standalone VMs and vTPM enabled. No candidate controller deployment has been validated: the provider's own approval path, certificate adoption, rotation adoption and drain behavior remain unexercised. Ubuntu, other GKE versions and arm64 are unevaluated.
- `enableVtpm: false` remains expressible but is known ineligible: without a vTPM there is no attestation key, so attested enrollment cannot succeed.
- IAM: `compute.instances.getShieldedInstanceIdentity` (new) and `compute.projects.get` for the controller service account, both in [`deploy/iam/karpenter-controller-role.yaml`](../deploy/iam/karpenter-controller-role.yaml).
- Kubernetes RBAC: the chart now grants `create` on `subjectaccessreviews`, plus the existing CSR approval permissions.
- Migration: existing NodeClaims have no recorded incarnation and cannot use attested enrollment. That includes native plugin reissuance: a registered claim launched by an earlier version cannot be re-issued through the bootstrap path, and only `system:node` continuity remains available where that path was in use. Replace these claims through normal consolidation/expiration or delete them before their certificates need renewal; do not backfill the annotation from a name lookup. See [MIGRATION.md](../MIGRATION.md).
- No automatic rollback: reverting to an older controller restores un-attested bootstrap approval.

## Test plan

### Implemented local regression tests

`pkg/controllers/csr` generates TPM proofs locally (software EC CSR key, local RSA attestation key, encoded TPM certify structure) with no real environment identifiers, keys or CSRs. They cover valid initial enrollment for both bootstrap usernames, valid re-enrollment and drain, missing/tampered/duplicate/unsupported/malformed envelopes (including malformed PEM blocks that a permissive decoder would skip), changed CSR key, wrong attestation key, wrong project/zone/name/incarnation, a requested instance organization with a wrong project number, zone or instance ID, missing or invalid recorded ID, unlaunched/deleting/registered-as-another-node claims, SAR allow/deny/error with original identity propagation, temporary identity failures that do not starve other requests, terminal and already-issued requests, approval conflicts, and ordinary self-renewal including drain.

`pkg/providers/instance` covers the identity lookup contract (fresh reads, incarnation-addressed key read, replacement rejection, missing VM, missing RSA key, temporary failures), launch incarnation recording from the terminal operation, the refusal to launch without a trusted incarnation or from an unowned same-name VM, regional ownership conflicts preventing a duplicate launch, ownership-label adoption behavior and legacy VMs. `pkg/providers/gke` covers the cached authoritative project number. `pkg/cloudprovider` covers annotation mapping and the incarnation-mismatch error.

### Added GKE integration coverage (not executed)

`test/suites/provisioning/csr.go` provisions one COS/amd64 on-demand vTPM node through the existing helpers and asserts that the NodeClaim records the numeric instance ID of the live VM, that the VM has vTPM enabled, that the native kubelet client CSR contains the four required attestation blocks and at most one optional attestation certificate, and that it was approved by `KarpenterAutoApprover` with a certificate issued. It does not fabricate rotation.

### Outstanding real-GKE validation

The following still requires a separately approved candidate deployment and was not executed:

1. Confirm the approved request is attributable to the candidate controller and the node adopts its certificate.
2. Exercise the installed native rotation path without exporting keys, and assert continued kubelet health.
3. Repeat under a PDB-held drain and verify the node stays healthy until release.
4. Verify tampered, different-key and cross-instance proofs cannot acquire a certificate, without claiming same-key replay rejection.
5. Evaluate Ubuntu and other supported profiles before widening the matrix.

## Acceptance criteria

- [X] Pass the native initial-enrollment and rotation compatibility gate.
- [X] Review the support/migration contract and the concrete incarnation association.
- [X] Validate cryptographic interoperability and authorization locally.
- [ ] Validate against real GKE enrollment, native reissuance and drain scenarios with a candidate deployment.

## Alternatives considered

- **Copy the entire Google approver:** includes MIG membership, serving certificates and stale-node cleanup with different ownership/lifecycle assumptions. Only proof verification and SAR semantics are reused.
- **Name, label or numeric-ID lookup alone:** establishes that a VM exists, not that it sent this CSR.
- **Compute instance identity JWT:** [Google-signed VM identity tokens](https://docs.cloud.google.com/compute/docs/instances/verifying-instance-identity) are useful identity evidence but are not bound to the CSR key by this bootstrap flow; transporting them would require a separately designed protocol.
- **Accept both attested and non-attested shared bootstrap requests:** preserves the original impersonation boundary and cannot be advertised as mandatory origin verification.

## Primary references

- [Pinned upstream approver](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gcp-controller-manager/node_csr_approver.go), [TPM helper](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/pkg/tpmattest/tpmattest.go), [request producer](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gke-exec-auth-plugin/request.go), [TPM certification](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gke-exec-auth-plugin/tpm.go) and [rotation](https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/cmd/gke-exec-auth-plugin/cache.go).
- [Shielded GKE prerequisites and MIG requirement](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/shielded-gke-nodes).
- [Compute signing-key identity API and IAM](https://docs.cloud.google.com/compute/docs/reference/rest/v1/instances/getShieldedInstanceIdentity).
- [go-tpm](https://github.com/google/go-tpm) (Apache-2.0), used through `legacy/tpm2` for TPM structure parsing and public-object naming.
