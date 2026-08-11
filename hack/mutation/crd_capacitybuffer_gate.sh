#!/usr/bin/env bash

set -eu -o pipefail

# GKE provides the CapacityBuffer CRD and controller natively starting at
# 1.35.2-gke.1842000 (active scaling scheme support; standby support follows
# later, at 1.36.0-gke.2253000). Installing our own copy on a cluster that
# already manages the CRD natively causes ownership conflicts, so only
# render it on clusters below the version where GKE starts providing it.
CRD=charts/karpenter-crd/templates/autoscaling.x-k8s.io_capacitybuffers.yaml

awk '
    {print}
    /  annotations:/ && !n {
        print "    helm.sh/resource-policy: keep"
        n++
    }
' "$CRD" > "$CRD.new" && mv "$CRD.new" "$CRD"

{
  echo '{{- if semverCompare "< 1.35.2-gke.1842000" .Capabilities.KubeVersion.Version }}'
  cat "$CRD"
  echo '{{- end }}'
} > "$CRD.new" && mv "$CRD.new" "$CRD"
