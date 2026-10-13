# Node Repair

Node repair replaces nodes that fail health checks. It is disabled by default and must be opted into via a feature gate.

> **Alpha:** The `nodeRepair` feature gate is off by default. Alpha behavior may change between releases.

## Enabling node repair

```yaml
controller:
  featureGates:
    nodeRepair: true
```

## How it works

With karpenter-core v1.15, repair is a voluntary disruption method. Once a monitored condition has remained unhealthy beyond its toleration, Karpenter checks the NodePool disruption budget and replacement feasibility. If replacement capacity is needed, it initializes the replacement before deleting the unhealthy NodeClaim.

GCP repair policies use replacement, not in-place reboot. They retain a zero termination-grace bound: after replacement staging, termination is forceful rather than waiting for normal PodDisruptionBudget or pod drain completion. Budgeted admission does not imply graceful draining.

Set `karpenter.sh/do-not-repair: "true"` on a Node to veto modern repair. Remove the annotation to allow repair again. A circuit breaker also pauses repair when more than 20% of a NodePool's nodes are unhealthy (rounded up).

### Legacy compatibility

To retain the pre-v1.15 forceful repair controller:

```yaml
controller:
  featureGates:
    nodeRepair: true
  settings:
    legacyNodeRepair: true
```

Legacy repair bypasses NodePool disruption budgets and does not use the modern repair veto or replacement-first staging. It still has an unhealthy-node circuit breaker. `terminateFirstRepair` has no effect in legacy mode.

## Monitored conditions

The conditions and tolerations are unchanged by the v1.15 migration:

| Condition                        | Trigger                                       | Toleration |
|----------------------------------|-----------------------------------------------|------------|
| `NodeReady=False`                | kubelet reports node not ready                | 10 minutes |
| `NodeReady=Unknown`              | kubelet unreachable                           | 10 minutes |
| `KernelDeadlock=True`            | GKE NPD: kernel hung task or deadlock         | 5 minutes  |
| `ReadonlyFilesystem=True`        | GKE NPD: root filesystem remounted read-only  | 5 minutes  |
| `FrequentKubeletRestart=True`    | GKE NPD: kubelet restarting too frequently    | 30 minutes |
| `FrequentContainerdRestart=True` | GKE NPD: containerd restarting too frequently | 30 minutes |

The four NPD conditions are set by [GKE Node Problem Detector](https://cloud.google.com/kubernetes-engine/docs/how-to/node-problem-detector). If NPD is disabled, repair can only react to `NodeReady` failures. Conditions that recover and become unhealthy again restart the toleration clock.

## Interaction with disruption budgets

Modern repair uses the `Unhealthy` disruption reason. A budget without `reasons` applies to repair as well as consolidation and drift; an all-reason `nodes: "0"` budget blocks repair of initialized nodes. The most restrictive matching budget wins: adding an `Unhealthy` budget does not override that block. Repair of registered but uninitialized nodes does not consume this budget.

For example, block consolidation and drift while permitting one repair at a time:

```yaml
spec:
  disruption:
    budgets:
      - nodes: "0"
        reasons: [Empty, Underutilized, Drifted]
      - nodes: "1"
        reasons: [Unhealthy]
```

Replacement staging can still block on unavailable capacity, scheduling constraints, or NodePool limits. `terminateFirstRepair` permits a static NodePool at its node limit to delete first so capacity can be refilled. Dynamic terminate-first requires reserved capacity, which the GCP provider does not yet support; it is not a general on-demand or Spot quota workaround.

## Troubleshooting

If repair does not proceed after the toleration:

1. Check `FEATURE_GATES` includes `NodeRepair=true` and determine whether `LEGACY_NODE_REPAIR` is enabled.
2. Check the condition has remained unhealthy, the Node has no `do-not-repair` veto, and the pool's circuit breaker has not tripped.
3. In modern mode, review budgets applying to `Unhealthy`, replacement scheduling, NodePool limits and cloud capacity.
4. Inspect controller logs and `DisruptionBlocked` events on the Node, NodeClaim and NodePool. Confirm controller Kubernetes permissions allow the lifecycle operations.

```sh
kubectl get events --field-selector reason=DisruptionBlocked
```

The 30-minute restart tolerations let transient process faults recover before replacement. Repeated repairs may indicate NPD thresholds or an underlying node problem that needs attention.
