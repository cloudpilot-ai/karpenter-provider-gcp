# Troubleshooting

## Nodes not provisioning

### Check Karpenter logs

Stream logs live:

```sh
kubectl logs -n karpenter-system deployment/karpenter --follow
```

Dump logs to a file for offline analysis:

```sh
kubectl logs -n karpenter-system deployment/karpenter > karpenter.log
# Include previous container logs if the pod has restarted
kubectl logs -n karpenter-system deployment/karpenter --previous >> karpenter.log
```

Look for lines containing `ERROR` or `failed`. Common causes: insufficient IAM permissions, quota exhaustion, no instance type matching the NodePool requirements.

### Check NodeClaim status

```sh
kubectl get nodeclaims
kubectl describe nodeclaim <name>
```

A NodeClaim stuck in `Pending` indicates Karpenter could not create an instance. The `status.conditions` section lists the reason.

### Pods still Pending after a node appears

The node may not have joined the cluster yet. Check:

```sh
kubectl get nodes
kubectl describe node <karpenter-node-name>
```

If the node never becomes `Ready`, check kubelet logs on the instance (via serial port or SSH if the node has a public IP or IAP access is configured).

---

## GCENodeClass not becoming Ready

```sh
kubectl describe gcenodeclass <name>
```

Check `status.conditions`. A common cause: `imageSelectorTerms` alias does not resolve to any image. Verify that the alias family and version are valid:

```yaml
imageSelectorTerms:
  - alias: ContainerOptimizedOS@latest
```

Supported families: `ContainerOptimizedOS`, `Ubuntu`.

---

## Custom machine types not discovered

Karpenter discovers instance types by querying the GCP `machineTypes.aggregatedList` API, which returns only predefined catalog types. Custom machine types (e.g. `n2-custom-8-24576`) are not returned by this API and are therefore not available for scheduling via standard `karpenter.sh/instance-type` label requirements.

This is a known limitation tracked in [GitHub issue #245](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/245).

---

## Private node clusters (org policy)

Karpenter correctly omits external IPs from **provisioned nodes** on clusters with `enablePrivateNodes: true`. However, Karpenter still creates zero-node bootstrap node pools at startup. On clusters where an org policy enforces private nodes (e.g. `container.managed.enablePrivateNodes`), this pool creation request may be rejected by GKE, preventing Karpenter from starting.

Full support for such clusters — eliminating bootstrap pool creation entirely — is tracked in [#230](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/230).

---

## CSR (Certificate Signing Request) not approved

Karpenter runs a CSR approver controller that automatically approves node bootstrap CSRs. If nodes fail to join with a TLS error, check whether CSR auto-approval is working:

```sh
kubectl get csr
```

Pending CSRs for Karpenter nodes should be approved automatically. If they are stuck in `Pending`, check the Karpenter controller logs for CSR-related errors and verify that the controller has the `certificates.k8s.io` RBAC permission to approve CSRs.

---

## Org policy rejections (ShieldedVM / CMEK)

Some organizations enforce GCP org policies that require Shielded VM or CMEK disk encryption on all instances. If instance creation fails with an org policy violation, configure the relevant fields in `GCENodeClass`:

**Shielded VM:**

```yaml
shieldedInstanceConfig:
  enableSecureBoot: true
  enableVtpm: true
  enableIntegrityMonitoring: true
```

**CMEK disk encryption:**

```yaml
disks:
  - category: pd-balanced
    sizeGiB: 60
    boot: true
    kmsKeyName: projects/my-project/locations/us-central1/keyRings/my-ring/cryptoKeys/my-key
```

---

## arm64 provisioning not available

arm64 node provisioning may be unavailable if the cluster's region does not support arm64 machine types or if arm64 template pools could not be created. Check Karpenter startup logs for related messages.

Karpenter detects arm64 architecture directly from the GCP Compute API. All arm64-capable instance families (`c4a`, `t2a`, `n4a`) are supported automatically.

Image resolution queries GCP image catalogs directly and does not depend on template pool availability, so arm64 images can be resolved even without arm64 template pools. However, the template pools are still used for network and kubelet configuration.

To verify arm64 machine types are available in your cluster's region:

```sh
gcloud compute machine-types list --zones=ZONE --filter="architecture:ARM64"
```

---

## Orphaned GCE instances

Karpenter runs a garbage-collection controller that periodically finds GCE instances tagged as Karpenter-managed but not tracked by any NodeClaim, and deletes them. This handles the case where a node was partially provisioned before a crash or where a NodeClaim was deleted without the underlying instance being cleaned up.

If you see unexpected GCE instances being terminated, check the Karpenter controller logs for GC-related messages:

```sh
kubectl logs -n karpenter-system deployment/karpenter | grep -i "garbage\|orphan"
```

GC only deletes instances that carry the Karpenter cluster tag and have no corresponding NodeClaim. Instances created outside Karpenter are not affected.

---

## Insufficient capacity errors

When Karpenter cannot provision an instance due to insufficient capacity (Spot or on-demand), it logs the error and marks the zone/instance-type/capacity-type combination as unavailable for 30 minutes. Karpenter skips these cached entries during zone selection, allowing the TTL to expire naturally so zones can recover and become available again.

If all zones for a given instance type are exhausted, Karpenter returns an insufficient capacity error immediately without attempting GCP API calls that would fail.

To increase provisioning success:

- Add more instance families to the NodePool `requirements`:

  ```yaml
  - key: karpenter.k8s.gcp/instance-family
    operator: In
    values: ["n4", "n2", "e2", "n2d"]
  ```

- Add more zones:

  ```yaml
  - key: topology.kubernetes.io/zone
    operator: In
    values: ["us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"]
  ```

- Allow both spot and on-demand to let Karpenter fall back automatically.
