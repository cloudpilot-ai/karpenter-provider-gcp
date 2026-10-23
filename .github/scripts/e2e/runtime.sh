#!/usr/bin/env bash
set -euo pipefail
# Called only by the trusted runner under its live canonical Lease/process group.
[[ $# == 6 && $1 == deploy && $5 =~ ^[a-f0-9]{40}$ ]] || exit 1
archive=$2 chart=$3 crd=$4 sha=$5 results=$6
: "${E2E_PROJECT_ID:?}" "${E2E_LOCATION:?}" "${E2E_CLUSTER_NAME:?}" "${E2E_IMAGE_REPOSITORY:?}"
: "${E2E_CONTROLLER_SERVICE_ACCOUNT:?}" "${E2E_NODE_SERVICE_ACCOUNT:?}" "${E2E_REGION:?}"
[[ $E2E_IMAGE_REPOSITORY =~ ^${E2E_REGION}-docker\.pkg\.dev/${E2E_PROJECT_ID}/[a-z0-9-]+/[a-z0-9-]+$ ]] || exit 1
[[ -d $results && ! -L $results && ! -e $results/controller-image.txt ]] || exit 1
for file in "$archive" "$chart" "$crd"; do [[ -f $file && ! -L $file ]] || exit 1; done
umask 077
auth=$(mktemp -d)
trap 'rm -rf -- "$auth"' EXIT
host=${E2E_IMAGE_REPOSITORY%%/*}
printf '{"credHelpers":{"%s":"gcloud"}}\n' "$host" > "$auth/config.json"
export DOCKER_CONFIG="$auth"
# A digest file is crane's standard push output; no custom archive/image parser.
crane push "$archive" "$E2E_IMAGE_REPOSITORY:e2e-$sha" --image-refs "$auth/image.txt"
image=$(cat "$auth/image.txt")
[[ $image == "$E2E_IMAGE_REPOSITORY@sha256:"* && ${image#*@sha256:} =~ ^[a-f0-9]{64}$ ]] || exit 1
digest=${image##*@}
helm upgrade --install karpenter-crd "$crd" --namespace karpenter-system --wait --timeout 2m
helm upgrade --install karpenter "$chart" --namespace karpenter-system --wait --timeout 5m \
  --set-string logLevel=debug \
  --set-string "controller.settings.projectID=$E2E_PROJECT_ID" \
  --set-string "controller.settings.clusterName=$E2E_CLUSTER_NAME" \
  --set-string "controller.settings.clusterLocation=$E2E_LOCATION" \
  --set controller.featureGates.spotToSpotConsolidation=true,controller.featureGates.nodeRepair=true,controller.featureGates.nodeOverlay=true,controller.featureGates.staticCapacity=true \
  --set-string "serviceAccount.annotations.iam\\.gke\\.io/gcp-service-account=$E2E_CONTROLLER_SERVICE_ACCOUNT" \
  --set controller.replicaCount=1,credentials.enabled=false \
  --set-string "controller.settings.defaultNodepoolServiceAccount=$E2E_NODE_SERVICE_ACCOUNT" \
  --set-string "controller.image.repository=$E2E_IMAGE_REPOSITORY" \
  --set-string controller.image.tag=prepared --set-string "controller.image.digest=$digest"
# Only a successful deployment publishes the exact reference for Go verification.
(set -o noclobber; printf '%s\n' "$image" > "$results/controller-image.txt")
