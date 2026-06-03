# PDCSI node-labeler machine/disk compatibility data

This directory contains the upstream PDCSI node-labeler ConfigMap:
`deploy/kubernetes/overlays/node-labeler/configmap.yaml` from
`kubernetes-sigs/gcp-compute-persistent-disk-csi-driver`.

Pinned upstream commit: `b6ce51692da2efe262acbc65132b15943eb2bfe0`.

Karpenter Provider GCP embeds this ConfigMap and reads
`data.machine-pd-compatibility.json` to compute `disk-type.gke.io/*` Node labels
for Karpenter-created GKE nodes.

Update process:

1. Fetch the upstream PDCSI repository at the intended commit.
2. Copy `deploy/kubernetes/overlays/node-labeler/configmap.yaml` to `node-labeler-configmap.yaml` in this directory.
3. Update the pinned commit above.
4. Run `go test ./pkg/providers/disktype ./pkg/providers/metadata ./pkg/providers/instance` and `make presubmit`.

This update is manual by design. Do not call it from `make update`; required checks must be reproducible without fetching upstream `master`.
