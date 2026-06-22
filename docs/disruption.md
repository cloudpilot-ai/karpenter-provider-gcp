# Disruption

Karpenter removes nodes that are no longer needed so your cluster tracks demand instead of accumulating idle capacity. Each NodePool controls this through its `spec.disruption` settings, which determine both *which* nodes are eligible for removal and *how quickly* Karpenter can remove them.

## Configuring disruption

Disruption is configured per NodePool under `spec.disruption`:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 30s
    budgets:
      - nodes: "10%"
```

| Field | Description | Default |
|-------|-------------|---------|
| `consolidationPolicy` | When Karpenter may consolidate a node. `WhenEmpty` removes only nodes with no workload pods. `WhenEmptyOrUnderutilized` also replaces underutilised nodes with cheaper, better-fitting ones. | `WhenEmptyOrUnderutilized` |
| `consolidateAfter` | How long a node must continuously qualify for consolidation before Karpenter acts. Use this to wait out short-lived idle periods. Set to `Never` to disable consolidation entirely. | `0s` |
| `budgets` | Limits on how many nodes in the NodePool may be disrupted at once, optionally scoped by reason (`Empty`, `Underutilized`, `Drifted`) and by schedule. | `10%` |

See the [NodePool reference](reference/nodepool.md#disruption) for the complete field schema.

## Empty node handling

A node becomes empty once its last workload pod is removed — for example, after you scale down or delete a deployment. Empty nodes are the cheapest to reclaim because there is nothing left to drain, but how soon Karpenter deletes one still depends on the NodePool's disruption settings:

- **`consolidationPolicy` must permit it.** Both `WhenEmpty` and `WhenEmptyOrUnderutilized` allow empty nodes to be removed. If consolidation is disabled (`consolidateAfter: Never`), empty nodes are kept.
- **`consolidateAfter` must elapse.** Karpenter waits for the node to stay empty for the full `consolidateAfter` window before deleting it. If the node receives new pods inside that window, the timer resets and the node is no longer a candidate.
- **The `Empty` disruption budget must allow it.** Empty-node deletion counts against the NodePool's disruption budget. Once the budget for the `Empty` reason is exhausted, deletions pause until capacity frees up.

This keeps empty-node removal aligned with the rest of the disruption controller rather than treating it as a special case. Setting a non-zero `consolidateAfter` is the recommended way to avoid premature node churn for workloads where nodes empty and refill frequently, such as CI runners or batch jobs.

## Disruption budgets

Disruption budgets cap how many of a NodePool's nodes Karpenter will voluntarily disrupt at the same time, spreading out replacement so workloads are not all rescheduled at once. A budget can apply to every disruption reason or be scoped to specific ones, and you can limit it to a recurring schedule:

```yaml
spec:
  disruption:
    budgets:
      # At most 10% of the NodePool's nodes disrupting at any time
      - nodes: "10%"
      # Allow no empty-node deletions during business hours on weekdays
      - reasons: ["Empty"]
        nodes: "0"
        schedule: "0 9 * * mon-fri"
        duration: 8h
```

When several budgets are active at once, Karpenter applies the most restrictive one. See the [Budget reference](reference/nodepool.md#budget) for the full field schema.

> **Note:** Disruption budgets govern *voluntary* disruption such as consolidation and drift. They do not apply to involuntary replacements — [node repair](node-repair.md) replaces an unhealthy node immediately regardless of the budget.
