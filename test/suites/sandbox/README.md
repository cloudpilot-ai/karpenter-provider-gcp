# Sandbox enablement E2Es

These smoke tests provision sandbox nodes and run Pods through an existing gVisor RuntimeClass. They reuse the environment's class/pool, launch/initialization, readiness, datapath and node-removal helpers.

| Case      | Machine          | Boot disk / pool CPU limit | Workload                                                             |
|-----------|------------------|----------------------------|----------------------------------------------------------------------|
| amd64 CPU | `n2-standard-2`  | 60 GiB / 2                 | Direct `/bin/uname -r`                                               |
| Arm64 CPU | `t2a-standard-2` | 60 GiB / 2                 | Direct `/bin/uname -r`                                               |
| amd64 L4  | `g2-standard-4`  | 100 GiB / 4                | `uname` plus the official CUDA vector-add sample, requesting one GPU |

The pinned Kubernetes busybox image supports both CPU architectures. Kernel output must differ from the node's host kernel; CUDA must exit successfully and log `Test PASSED`. Containers are one-shot (`RestartPolicyNever`): tests wait for successful completion, not whole-Pod Ready. There are no embedded scripts. This corroborates runtime enablement, not comprehensive isolation, networking or storage correctness.

## Prerequisites and selection

- Satisfy the [operator prerequisites](../../../docs/sandbox-nodes.md), including matching controller/CRDs, IAM, artifact access and reviewed bootstrap compatibility.
- Provide a GKE/admin-owned RuntimeClass (any name) with handler `gvisor`, only the `sandbox.gke.io/runtime: gvisor` node selector, and a toleration matching its `NoSchedule` taint. Discovery is read-only; missing prerequisites fail rather than skip. Tests never create RuntimeClasses or native pools.
- Supply zonal capacity/quota for the pinned types. The L4 case needs the [GPU software stack/device plugin](../../../docs/gpu-nodes.md); required DaemonSets must tolerate the sandbox and test-pool taints.

All cases carry `suite:sandbox`; only CUDA also carries `suite:gpu`. `standard` selects the two CPU cases, `gpu` includes CUDA, and `--selection sandbox` selects all three. After separate approval for the target environment, the existing live runner can select them:

```bash
go run ./hack/e2e-runner --selection sandbox
```

**Do not use the native runner for offline discovery:** it acquires a Kubernetes Lease even when Ginkgo's `--dry-run` is passed after `--`. Offline compilation is `go test ./test/... -run '^$'`; it does not execute the specs.

Each case owns a unique Pod/pool/class, uses on-demand capacity with Never consolidation/expiry and an all-reason zero disruption budget, and deletes its Pod before the pool/class with normal finalizers. CPU limits/budgets are operational guards, not hard allocation or billing guarantees.

The suite intentionally excludes custom DNS/TLS/filesystem probes, ordinary-Pod rejection and live ownership/disable/drift checks. Existing unit tests cover ownership and lifecycle decisions, not their end-to-end behavior. Offline compilation/discovery does not prove live node boot, sandbox execution or CUDA success.
