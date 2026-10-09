# PDCSI node-labeler machine/disk compatibility data

This directory contains the upstream PDCSI node-labeler ConfigMap:
`deploy/kubernetes/overlays/node-labeler/configmap.yaml` from
`sigs.k8s.io/gcp-compute-persistent-disk-csi-driver`.

Karpenter Provider GCP embeds this ConfigMap and reads
`data.machine-pd-compatibility.json` to compute `disk-type.gke.io/*` Node labels
and scheduler requirements for Karpenter-created GKE nodes.

`gen4-pd-compatibility.patch` backports upstream CSI driver [PR #2384](https://github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver/pull/2384), commit `2b123788da2550b6ff03533fc127d35ab0246604`. It removes unsupported `pd-balanced` and `pd-ssd` entries from `c4a` and `n4`.

Update process:

1. Dependabot updates the pinned `sigs.k8s.io/gcp-compute-persistent-disk-csi-driver`
   module version in `hack/pdcsi-data/go.mod`.
2. Run `make update`; the `update-pdcsi-compatibility` target copies the node-labeler ConfigMap from the downloaded module into this directory and applies the backport. If the module already contains the correction, the target verifies that the reverse patch applies. Otherwise, a patch mismatch fails the update.
3. Commit the updated `node-labeler-configmap.yaml` with the data module bump.
4. Run `go test ./pkg/providers/disktype ./pkg/providers/instancetype ./pkg/providers/instance` and `make presubmit`.

Do not fetch upstream `master` directly. The checked-in compatibility data must come from the module version recorded in `hack/pdcsi-data/go.mod` plus the checked-in backport so updates are reproducible. Remove the patch and its Makefile step when the pinned module includes the upstream correction.
