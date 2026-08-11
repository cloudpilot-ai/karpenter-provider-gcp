#!/usr/bin/env bash

set -eu -o pipefail

# Ships the CapacityBuffer CRD via the main karpenter chart's templates/ dir
# (not the unconditional crds/ dir, which can't be templated) so it can be
# gated by both controller.featureGates.capacityBuffer and the cluster's
# Kubernetes version. See hack/mutation/crd_capacitybuffer_gate.sh for the
# karpenter-crd chart's version-only gate.
KARPENTER_CRD_DIR=vendor/sigs.k8s.io/karpenter/pkg/apis/crds
CRD=charts/karpenter/templates/autoscaling.x-k8s.io_capacitybuffers.yaml

cp "${KARPENTER_CRD_DIR}/autoscaling.x-k8s.io_capacitybuffers.yaml" "$CRD"

awk '
    {print}
    /  annotations:/ && !n {
        print "    {{- with .Values.additionalAnnotations }}"
        print "    {{- toYaml . | nindent 4 }}"
        print "    {{- end }}"
        n++
    }
' "$CRD" > "$CRD.new" && mv "$CRD.new" "$CRD"

{
  echo '{{- if and .Values.controller.featureGates.capacityBuffer (semverCompare "< 1.35.2-gke.1842000" .Capabilities.KubeVersion.Version) }}'
  cat "$CRD"
  echo '{{- end }}'
} > "$CRD.new" && mv "$CRD.new" "$CRD"
