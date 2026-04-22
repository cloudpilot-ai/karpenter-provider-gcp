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

## Private node clusters

Karpenter detects cluster-level private nodes automatically by reading `DefaultEnablePrivateNodes` and `PrivateClusterConfig.EnablePrivateNodes` from the GKE API. When either flag is set, the fallback pool (`karpenter-default`) is created with `EnablePrivateNodes: true`. No extra configuration is needed.

For selectively provisioning nodes without a public IP on a public cluster, see [Networking examples — Private nodes](examples/networking.md#private-nodes-no-external-ip).

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

arm64 node provisioning is unavailable if no arm64 machine types exist in the cluster's region. Check Karpenter startup logs for instance type discovery errors.

If arm64 support is needed, verify that arm64 machine types (e.g. `c4a`, `t2a`) are available in your cluster's region:

```sh
gcloud compute machine-types list --zones=ZONE --filter="name:t2a OR name:c4a"
```

---

## Insufficient capacity errors

When Karpenter cannot provision an instance due to insufficient capacity (spot or on-demand), it logs the error and marks the zone/instance-type combination as unavailable for a short backoff period before retrying.

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
