# Bootstrap Pool Selection

Karpenter requires a GKE node pool to read bootstrap metadata (instance templates and kubelet settings). Rather than creating dedicated template pools, Karpenter discovers and reuses an existing cluster pool automatically.

## How it works

On startup and every 12 minutes, Karpenter runs pool discovery:

1. **Pinned pool** (if `DEFAULT_NODEPOOL_TEMPLATE_NAME` is set): Use that pool exclusively. Return an error if it does not exist or is not RUNNING.
2. **default-pool**: If the cluster's `default-pool` exists and is RUNNING or RUNNING_WITH_ERROR, use it.
3. **Alphabetical fallback**: Sort all remaining RUNNING or RUNNING_WITH_ERROR pools by name and use the first one.
4. **No eligible pool**: If no pool passes the above checks, Karpenter creates a minimal `karpenter-fallback` pool and polls every 15 seconds until it becomes RUNNING.

A pool is eligible when its status is `RUNNING` or `RUNNING_WITH_ERROR`. Pools in `PROVISIONING`, `STOPPING`, `ERROR`, or `RECONCILING` states are skipped.

## Pinning a specific pool

Set the `DEFAULT_NODEPOOL_TEMPLATE_NAME` environment variable or Helm value to lock Karpenter to a specific pool:

```yaml
controller:
  settings:
    defaultNodePoolTemplateName: "my-system-pool"
```

When set, Karpenter uses only that pool and returns an error if it is not RUNNING. This is useful when:

- Your cluster has multiple pools and you want deterministic bootstrap metadata.
- Org policies require a pre-approved pool configuration.
- You want to avoid the alphabetical fallback behaviour.

## The fallback pool

When no RUNNING pool exists, Karpenter creates a zero-node pool named `karpenter-fallback`. This pool is hardened against common GCP org policy constraints:

| Constraint                                             | Fallback pool setting                                    |
|--------------------------------------------------------|----------------------------------------------------------|
| `compute.requireShieldedVm`                            | Shielded VM enabled (Secure Boot + Integrity Monitoring) |
| `container.managed.enablePrivateNodes`                 | Mirrors cluster's private-node setting                   |
| `container.managed.disableInsecureKubeletReadOnlyPort` | Insecure read-only port disabled                         |
| `container.managed.enableWorkloadIdentityFederation`   | GKE_METADATA mode when Workload Identity is active       |
| `compute.managed.blockProjectSshKeys`                  | block-project-ssh-keys metadata set                      |

The `gcp.restrictNonCmekServices` constraint cannot be auto-satisfied because it requires a customer-managed KMS key. On clusters with this policy, pre-create a RUNNING pool that meets your org's requirements and set `DEFAULT_NODEPOOL_TEMPLATE_NAME`.

## Cross-OS and cross-architecture provisioning

Karpenter patches the source pool's kube-env metadata when provisioning nodes with a different OS or architecture:

- **OS mismatch**: `PatchKubeEnvForOSType` adjusts kube-env fields when the target image (Ubuntu vs COS) differs from the source pool.
- **Architecture mismatch**: `PatchKubeEnvForArch` adjusts kube-env when provisioning arm64 nodes from an amd64 source pool or vice versa.

This allows a single source pool to bootstrap nodes of any OS and architecture combination.

## Metadata sources

Karpenter reads kubelet bootstrap metadata from the bootstrap pool's instance template. Other instance settings are derived from GCENodeClass, cluster config, or Karpenter/provider defaults.

| Metadata                               | Source                                                                                                                                             |
|----------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| Kubelet configuration (max-pods, etc.) | Bootstrap pool template, then patched by GCENodeClass settings                                                                                     |
| Subnetwork                             | GCENodeClass `spec.networkConfig.subnetwork`; defaults to cluster subnetwork from GKE cluster config                                               |
| Private-node setting                   | GCENodeClass `spec.networkConfig.enablePrivateNodes`; defaults to cluster private-node setting from GKE cluster config                             |
| Service account                        | GCENodeClass `spec.serviceAccount`, `DEFAULT_NODEPOOL_SERVICE_ACCOUNT`, or Compute Engine default service account                                  |
| Service account scopes                 | Karpenter sets `cloud-platform`                                                                                                                    |
| Node labels                            | Rebuilt from Karpenter/provider target state, plus a known set of GKE readiness-gate labels carried over from the source pool template (see below) |
| Node taints                            | Rebuilt from Karpenter/provider target state                                                                                                       |

Labels and taints configured on the bootstrap pool (such as `workload=karpenter` or `dedicated=karpenter:NoSchedule`) are intentionally discarded. Karpenter-provisioned nodes receive only labels and taints that Karpenter explicitly controls, plus the GKE readiness-gate labels described below.

### GKE readiness-gate labels

GKE gates its system DaemonSets on per-node readiness labels. As an exception to the discard rule above, Karpenter carries these labels over from the source pool's template so those DaemonSets schedule on provisioned nodes. Karpenter-owned labels still take precedence. The preserved keys are:

- `projectcalico.org/ds-ready`
- `node.kubernetes.io/kube-proxy-ds-ready`
- `iam.gke.io/gke-metadata-server-enabled`
- `node.kubernetes.io/masq-agent-ds-ready`
- `cloud.google.com/gke-netd-ready`
- `addon.gke.io/node-local-dns-ds-ready`

## Upgrading from template pools

Previous Karpenter versions created up to four template pools (`karpenter-default`, `karpenter-ubuntu`, `karpenter-cos-arm64`, `karpenter-ubuntu-arm64`) at startup. These are no longer needed.

After upgrading to a version with bootstrap pool discovery:

1. Confirm provisioning works correctly with the new version.
2. Delete the legacy pools at your own pace:

```bash
for pool in karpenter-ubuntu karpenter-cos-arm64 karpenter-ubuntu-arm64 karpenter-default; do
  gcloud container node-pools delete "$pool" \
    --cluster=CLUSTER_NAME \
    --location=CLUSTER_LOCATION \
    --quiet
done
```

The new fallback pool is named `karpenter-fallback`, so the above command is safe and unambiguous.

Rolling back to the previous version will re-create the legacy pools automatically.

## Troubleshooting

**Karpenter cannot find a bootstrap pool**

Check controller logs for the discovery error:

```sh
kubectl logs -n karpenter-system -l app.kubernetes.io/name=karpenter | grep "bootstrap"
```

Common causes:

- All pools are in a non-RUNNING state (provisioning, upgrading, or stopped).
- `DEFAULT_NODEPOOL_TEMPLATE_NAME` is set to a pool that does not exist or is not RUNNING.
- The cluster has no node pools at all (unlikely in practice).

**Fallback pool creation fails**

If fallback creation fails due to org policy violations, the error message identifies the blocking constraint. Create a compliant pool manually and set `DEFAULT_NODEPOOL_TEMPLATE_NAME` to that pool's name.

**Nodes fail to join the cluster after switching bootstrap pools**

If the new source pool has materially different kubelet bootstrap metadata, existing Karpenter nodes may have stale configuration. Trigger a rolling replacement:

```bash
kubectl annotate nodepool NODEPOOL_NAME "karpenter.k8s.gcp/force-rollout=$(date +%s)" --overwrite
```
