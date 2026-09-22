# Stage 1 local E2E runner

- Complete runtime inventory/resource-profile validation, including unknown and zero-test rejection plus CLI/report goldens.
- Complete the fail-closed quota planner: snapshot freshness, regional/global CPU, IPs, disks, GPUs, existing usage, reserves, caps, deterministic waves, and per-suite workers.
- Finish lifecycle protection: target lock and Kubernetes Lease, alignment checks, between-wave quota/owned-resource checks, cancellation/lease-loss propagation, and primary-error preservation.
- Complete controller log/event and metrics collection, including availability and coverage reporting.
- Finish Make/CI integration; retain existing setup/deploy/check-clean/teardown behavior.
- Rerun full local validation (`make presubmit`) and focused production dead-code checks.
- Rerun the authorized `standard` preset in the isolated `stage1-local-e2e` environment. The prior run passed 53/55 specs; Ubuntu arm64 Spot timed out during suite cleanup. The runner’s incorrect zero exit status is fixed locally.
- Compare the final diff to `.pi/plans/stage-1-local-e2e-tool.md`, obtain fresh review, update the plan ledger, then stop for review/merge.
