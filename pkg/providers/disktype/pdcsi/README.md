# PDCSI node-labeler machine/disk compatibility data

This directory contains the upstream PDCSI node-labeler ConfigMap:
`deploy/kubernetes/overlays/node-labeler/configmap.yaml` from
`sigs.k8s.io/gcp-compute-persistent-disk-csi-driver`.

Karpenter Provider GCP embeds this ConfigMap and reads
`data.machine-pd-compatibility.json` to compute `disk-type.gke.io/*` Node labels
and scheduler requirements for Karpenter-created GKE nodes.

Update process:

1. Dependabot updates the pinned `sigs.k8s.io/gcp-compute-persistent-disk-csi-driver`
   module version in `hack/pdcsi-data/go.mod`.
2. Run `make update`; the `update-pdcsi-compatibility` target copies the
   node-labeler ConfigMap from the downloaded module into this directory.
3. Commit the updated `node-labeler-configmap.yaml` with the data module bump.
4. Run `go test ./pkg/providers/disktype ./pkg/providers/metadata ./pkg/providers/instancetype ./pkg/providers/instance` and `make presubmit`.

Do not fetch upstream `master` directly. The checked-in compatibility data must
come from the module version recorded in `hack/pdcsi-data/go.mod` so updates are reproducible.
