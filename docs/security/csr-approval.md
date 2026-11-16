# CSR approval security contract

Karpenter approves kubelet-client certificate signing requests (CSRs) for its GCE nodes. Approval is an authorization decision: matching a signer or certificate subject does not establish that the requester may obtain that identity.

## Approval policy

The controller validates the CSR signature, kubelet-client signer, allowed usages and subject. It rejects subject alternative names and organizations outside `system:nodes` and the supported GKE node-pool/instance organizations. Requested GKE node-pool groups must already belong to the API-authenticated requester; renewal instance groups must also already belong to that requester.

Eligible NodeClaims must reference `karpenter.k8s.gcp/GCENodeClass` and have a complete `gce://<project>/<zone>/<instance>` ProviderID matching the requested node name.

- **Bootstrap:** the requester is `kubelet-nodepool-bootstrap`; the NodeClaim has `Launched=True`, has not registered and is not being deleted. A supplied GKE instance organization's zone must match the claim.
- **Renewal:** the requester is `system:node:<requested-name>` and belongs to the authenticated `system:nodes` group. The NodeClaim has `Registered=True`, and its node name and GCE instance name match the request. Self-renewal remains allowed while that registered claim is draining/deleting, so a long drain does not lose its kubelet identity.

The requester username/groups are those recorded by the Kubernetes API server, not identity assertions in the requested certificate. A failed NodeClaim read prevents approval. Already approved or denied CSRs are not changed. Unrelated requests remain pending for their proper approver or an administrator.

## Remaining trust boundary

**This policy does not authenticate the requester as the actual GCE VM.** GKE source pools share a bootstrap username. A holder of one of those bootstrap credentials can still request a pending Karpenter node's identity before registration. Lifecycle, name and zone checks narrow that exposure but do not close it. A Compute lookup confirming an attacker-supplied instance ID would not, by itself, prove requester identity either.

Closing that boundary requires separately designed and validated instance-bound enrollment, including real bootstrap and certificate rotation. This change does not implement such a protocol or revoke certificates already issued. Deleting a CSR does not revoke its certificate. Another approver may also approve requests independently; these controller checks are not a claim about every cluster approval path.

## Regression tests

Run the focused offline authorization tests:

```sh
go test -race ./pkg/controllers/csr
```

They cover approval decisions and the approval API write using a local HTTP test endpoint and a small read-client double. They do not exercise real API admission, CA signing or GKE joining/rotation. Real GKE bootstrap and rotation must be validated separately before release, in an explicitly approved environment.

## Chart certificate permissions

The controller's chart grants:

- `get`, `list`, `watch` on CSRs;
- `update` on the approval subresource;
- `approve` only for `kubernetes.io/kube-apiserver-client-kubelet`.

These permissions are bound to the controller ServiceAccount. External cluster roles, group memberships, bindings and aggregation inputs require a separate authorized exposure audit. Existing gosec/CodeQL checks and Greptile review guidance are complementary, not proofs of correct authorization.
