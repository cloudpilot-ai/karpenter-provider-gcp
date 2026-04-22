# Node Repair

Node repair automatically replaces nodes that fail health checks. It is disabled by default and must be opted into via a feature gate.

## Enabling node repair

Set the `nodeRepair` feature gate when installing or upgrading Karpenter:

```sh
helm upgrade karpenter karpenter-provider-gcp/karpenter --install \
  --namespace karpenter-system \
  --set "controller.featureGates.nodeRepair=true" \
  ...
```

Or in `values.yaml`:

```yaml
controller:
  featureGates:
    nodeRepair: true
```

## How it works

Karpenter polls node conditions on a short interval. When a condition matches a repair policy — and has been in the unhealthy state continuously for at least the configured toleration duration — Karpenter cordons the node, drains it, and replaces it with a fresh one.

The toleration duration prevents flapping: transient blips that recover quickly do not trigger replacement.

## Monitored conditions

| Condition                        | Trigger                                       | Toleration |
|----------------------------------|-----------------------------------------------|------------|
| `NodeReady=False`                | kubelet reports node not ready                | 10 minutes |
| `NodeReady=Unknown`              | kubelet unreachable (e.g. OOM, crash)         | 10 minutes |
| `KernelDeadlock=True`            | GKE NPD: kernel hung task or deadlock         | 5 minutes  |
| `ReadonlyFilesystem=True`        | GKE NPD: root filesystem remounted read-only  | 5 minutes  |
| `FrequentKubeletRestart=True`    | GKE NPD: kubelet restarting too frequently    | 30 minutes |
| `FrequentContainerdRestart=True` | GKE NPD: containerd restarting too frequently | 30 minutes |

`KernelDeadlock`, `ReadonlyFilesystem`, `FrequentKubeletRestart`, and `FrequentContainerdRestart` are set by [GKE Node Problem Detector](https://cloud.google.com/kubernetes-engine/docs/how-to/node-problem-detector), which runs by default on all GKE Standard node pools. If NPD is disabled on your node pools, those four conditions are never set and repair will only fire on `NodeReady` failures.

## Verifying repair is active

Karpenter logs the repair action before cordoning:

```
INFO  node repair triggered  {"node": "karpenter-abc12", "condition": "KernelDeadlock", "conditionStatus": "True"}
```

You can also watch NodeClaim events:

```sh
kubectl get events --field-selector involvedObject.kind=NodeClaim
```

## Interaction with disruption budgets

Node repair bypasses the NodePool disruption budget — an unhealthy node is replaced immediately regardless of `disruption.budgets`. This matches the intent: disruption budgets govern voluntary disruption (consolidation, drift), not involuntary failures.

## Troubleshooting

**Repair does not fire after a node becomes unhealthy**

1. Confirm the feature gate is set: check the controller pod's `--feature-gates` flag or the Helm release values.
2. Check that the condition has persisted beyond the toleration duration — conditions that clear and re-set reset the clock.
3. Verify Karpenter has permission to delete NodeClaims and cordon nodes.

**Node is replaced too aggressively**

Transient OS issues can trigger `FrequentKubeletRestart` or `FrequentContainerdRestart` before the process stabilises. The 30-minute toleration on those conditions gives most workloads time to self-heal. If replacements are still too frequent, consider whether NPD thresholds on the node pool need tuning.
