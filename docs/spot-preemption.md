# Spot preemption notice

GCE reclaims Spot VMs with little warning. By default Karpenter only finds out once the shutdown signal has already been delivered and kubelet reports the node as shutting down, which leaves a fraction of the 30 second shutdown window for draining.

This page covers how to get that warning earlier — up to two minutes earlier — so pods have time to move.

## How it works

Two pieces, and they are useful independently:

1. **`GCENodeClass.spec.preemptionNoticeDuration`** asks GCE to flip the `instance/preempted` metadata key ahead of the shutdown signal instead of at the same moment. Set it to `120` for two minutes of warning.
2. **The preemption notice detector**, an optional DaemonSet in the Karpenter chart, watches that metadata key on each Spot node and sets a `GCESpotPreempting` node condition when it flips.

Karpenter's interruption controller watches for `GCESpotPreempting=True` and immediately deletes the NodeClaim, which starts the drain. It also marks that instance type and zone unavailable for a while, so replacement capacity is not requested from the same place GCE just reclaimed.

Without the detector, nothing reads the metadata key and setting `preemptionNoticeDuration` on its own changes nothing that Karpenter can see.

## Enabling it

Turn on the detector:

```sh
helm upgrade karpenter karpenter-provider-gcp/karpenter --install \
  --namespace karpenter-system \
  --set "spotPreemptionNotice.enabled=true" \
  ...
```

Then ask for the notice on the NodeClasses that provision Spot capacity:

```yaml
apiVersion: karpenter.k8s.gcp/v1alpha1
kind: GCENodeClass
metadata:
  name: spot
spec:
  preemptionNoticeDuration: 120
```

Valid values are `0` (no advance notice, the default) and `120`. The field only affects Spot capacity — it is ignored when provisioning on-demand nodes, because those are never preempted.

Changing `preemptionNoticeDuration` drifts existing nodes. GCE only accepts the setting when an instance is created, so Karpenter has to replace nodes to apply it.

## Why it is off by default

GKE Standard node pools already run their own node-problem-detector, and Karpenter's [node repair](node-repair.md) feature depends on the conditions it sets. Enabling `spotPreemptionNotice` adds a second node-problem-detector alongside it.

The two do not conflict — they set different conditions and the GKE-managed one cannot be reconfigured to add custom checks — but it is a second agent on the node, so it is opt-in rather than automatic. The detector only lands on nodes matching its node selector, which defaults to Spot capacity only.

## Detector configuration

| Value                                     | Default                            | Purpose                                               |
|-------------------------------------------|------------------------------------|-------------------------------------------------------|
| `spotPreemptionNotice.enabled`            | `false`                            | Deploy the DaemonSet                                  |
| `spotPreemptionNotice.nodeSelector`       | `karpenter.sh/capacity-type: spot` | Which nodes run it                                    |
| `spotPreemptionNotice.waitTimeoutSeconds` | `30`                               | How long each metadata poll blocks                    |
| `spotPreemptionNotice.priorityClassName`  | `system-node-critical`             | Keeps it from being evicted off a node it is watching |
| `spotPreemptionNotice.image.tag`          | `v0.8.20`                          | node-problem-detector version                         |
| `spotPreemptionNotice.resources`          | 10m CPU / 32Mi                     | Per-node cost                                         |

The detector runs with host networking. It has to: pods otherwise reach the GKE metadata server, which does not expose `instance/preempted`. It talks to `169.254.169.254` directly rather than resolving `metadata.google.internal`, so detection does not depend on cluster DNS still working during a shutdown.

## Checking it works

Confirm GCE accepted the setting on a provisioned node:

```sh
gcloud compute instances describe <instance-name> --zone <zone> \
  --format="value(scheduling.preemptionNoticeDuration)"
```

Confirm the detector is running and reporting:

```sh
kubectl get pods -n karpenter-system -l app.kubernetes.io/name=karpenter-preemption-notice
kubectl get nodes -o custom-columns=\
'NAME:.metadata.name,PREEMPTING:.status.conditions[?(@.type=="GCESpotPreempting")].status'
```

A healthy node reports `False`. When a preemption notice arrives it flips to `True`, and Karpenter records a `PreemptionNoticeReceived` event against the node:

```sh
kubectl get events --field-selector reason=PreemptionNoticeReceived
```

## Fallback

The original detection path still runs. If the detector is not deployed, fails, or misses a notice, Karpenter falls back to the kubelet `NodeReady=False` / `node is shutting down` condition exactly as before. That path drains later and does not mark the offering unavailable, but nothing is lost by having the detector off.

## Troubleshooting

**The condition never appears on any node**

Check the detector pod is scheduled on the node in question. Its node selector defaults to `karpenter.sh/capacity-type: spot`, so it will not run on on-demand nodes — which is correct, since those are never preempted.

**The detector pod logs metadata request failures**

The pod needs host networking to reach the real metadata server. Confirm `hostNetwork: true` survived any values overrides, and that no network policy blocks `169.254.169.254`.

**Nodes drain late even with the detector running**

Check `preemptionNoticeDuration` is actually set on the instance with the `gcloud` command above. If it reads empty, the node was created before the field was set and needs replacing. With no notice duration the key still flips slightly earlier than the kubelet condition, but only by seconds rather than minutes.
