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

Check `status.conditions`. The `ImagesReady` condition shows whether Karpenter can resolve the configured image.

**Common causes:**

- **Invalid version format** — Pinned versions must match the family's format: `milestone.build.build.build` for ContainerOptimizedOS (e.g. `125.19216.104.126`), `vYYYYMMDD` for Ubuntu (e.g. `v20260416`). Invalid formats are rejected at admission.

- **Version not found** — If `ImagesReady` shows `ImageResolutionFailed`, the pinned version does not exist in GCP. Verify availability using the `gcloud compute images list` commands in [Image management](image-management.md#finding-available-versions).

- **Unsupported family** — Only `ContainerOptimizedOS` and `Ubuntu` are supported.

Example using the recommended structured fields:

```yaml
imageSelectorTerms:
  - family: ContainerOptimizedOS
    channel: cluster
```

The `alias` field (e.g. `alias: ContainerOptimizedOS@latest`) is deprecated — use `family`/`channel`/`version` for new configurations.

See [Image management](image-management.md) for version pinning options and format details.

---

## Custom machine types not discovered

Karpenter discovers instance types by querying the GCP `machineTypes.aggregatedList` API, which returns only predefined catalog types. Custom machine types (e.g. `n2-custom-8-24576`) are not returned by this API and are therefore not available for scheduling via standard `karpenter.sh/instance-type` label requirements.

This is a known limitation tracked in [GitHub issue #245](https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/245).

---

## Private node clusters (org policy)

Karpenter correctly omits external IPs from **provisioned nodes** on clusters with `enablePrivateNodes: true`. No extra configuration is required.

Karpenter discovers an existing RUNNING node pool for bootstrap metadata rather than creating new pools. If no pool exists and Karpenter must create the `karpenter-fallback` pool, it mirrors the cluster's private-node setting automatically so clusters with `container.managed.enablePrivateNodes` org policy are supported. See [Bootstrap pool selection](bootstrap-pool.md).

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

## Confidential VM rejected by GCE

Confidential VM is only supported on specific machine families. See the [GCP support matrix](https://cloud.google.com/confidential-computing/confidential-vm/docs/os-and-machine-type) for which families support which confidential type. If `confidentialInstanceType` is set on a `GCENodeClass` whose requirements admit incompatible families, GCE rejects the instance create call and the error appears on the `NodeClaim` `Launched` condition.

Constrain the family in the matching `NodePool` requirements. Example:

```yaml
requirements:
  - key: karpenter.k8s.gcp/instance-family
    operator: In
    values: [n2d]
```

Karpenter also forces `scheduling.onHostMaintenance` to `TERMINATE` for these instances because Confidential VMs cannot live-migrate.

The default `ContainerOptimizedOS` and `Ubuntu` images work for CPU Confidential VMs. GPU Confidential VMs (an A3 instance with an attached H100 GPU using Intel TDX) require a TDX-specific image that the `family` image selectors do not provide; pin one by its full resource URL with `imageSelectorTerms[].id` per the [GCP supported configurations](https://cloud.google.com/confidential-computing/confidential-vm/docs/supported-configurations#supported-images-gpu).

---

## arm64 provisioning not available

arm64 node provisioning may be unavailable if the cluster's region does not support arm64 machine types. Check Karpenter startup logs for related messages.

Karpenter detects arm64 architecture directly from the GCP Compute API. All arm64-capable instance families (`c4a`, `t2a`, `n4a`) are supported automatically.

Image resolution queries GCP image catalogs directly and does not depend on the bootstrap pool. Bootstrap pool discovery and kube-env patching allow a single source pool (of any architecture) to bootstrap nodes of any OS and architecture combination. See [Bootstrap pool selection](bootstrap-pool.md) for details.

To verify arm64 machine types are available in your cluster's region:

```sh
gcloud compute machine-types list --zones=ZONE --filter="architecture:ARM64"
```

---

## Idle node not being removed

Empty-node removal is handled entirely by Karpenter's upstream disruption controller — the GCP provider no longer runs a separate empty-node cleanup loop. A node is removed automatically only when it is empty or underutilized according to the matching NodePool's `spec.disruption` settings.

If an idle node is not being removed, check the following:

- **`consolidateAfter` and disruption budgets.** Karpenter waits for `consolidateAfter` to elapse before disrupting a node, and disruption budgets can block or rate-limit removals. A budget that sets `nodes: "0"` for the `Empty` reason prevents empty-node removal entirely. Review the NodePool's `spec.disruption` to confirm the timing and budgets are what you expect.

- **`consolidationPolicy`.** `WhenEmpty` removes only nodes that have no reschedulable pods. A node that still runs a Deployment-owned system pod — such as the GKE `konnectivity-agent` — is not considered empty by Karpenter core, because the pod is not DaemonSet overhead. Such a node remains until it can be consolidated as underutilized. Use `WhenEmptyOrUnderutilized` if you want Karpenter to consider removing or replacing these lightly loaded nodes.

  ```yaml
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 30s
  ```

Check the current disruption settings on your NodePools with:

```sh
kubectl get nodepools -o yaml
```

See the [NodePool disruption reference](reference/nodepool.md#disruption) for the full list of settings.

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

---

## Zone mismatch errors

Karpenter provisions nodes only in zones configured for your GKE cluster. If a NodePool's `topology.kubernetes.io/zone` requirement specifies zones outside the cluster's configured locations, provisioning fails with an error like:

```
no zones match topology requirement "topology.kubernetes.io/zone"; requested zones europe-west4-b, configured GKE cluster locations europe-west4-a,europe-west4-c
```

Common cause:

- The NodePool requirement lists zones not in the cluster's node locations. Zonal clusters can use multiple node locations, but the requested zones must be configured on the cluster.

To resolve:

1. Check your cluster's configured node locations:

   ```sh
   gcloud container clusters describe CLUSTER_NAME \
     --location=CLUSTER_LOCATION \
     --format="value(locations)"
   ```

2. Update the NodePool `topology.kubernetes.io/zone` requirement to match available zones, or remove the requirement to let Karpenter use any configured cluster zone.
