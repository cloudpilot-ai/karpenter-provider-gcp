#!/usr/bin/env bash
set -euo pipefail

IMAGE=${1:?digest-qualified image reference required}
if [[ ! "$IMAGE" =~ ^public\.ecr\.aws/cloudpilotai/gcp/karpenter@sha256:[a-f0-9]{64}$ ]]; then
  echo 'Expected exactly one digest-qualified controller image from ko build' >&2
  exit 1
fi
: "${GITHUB_WORKFLOW_REF:?GitHub workflow identity required}"

# Public ECR cannot be assumed to support OCI 1.1 referrers. Keep the
# digest-derived .sig tag layout and matching verifier until registry migration.
cosign sign --yes --new-bundle-format=false --use-signing-config=false \
  --registry-referrers-mode=legacy "$IMAGE"
cosign verify --new-bundle-format=false --experimental-oci11=false \
  --certificate-identity "https://github.com/$GITHUB_WORKFLOW_REF" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com "$IMAGE"

printf 'image=%s\ndigest=%s\n' "$IMAGE" "${IMAGE##*@}" >> "${GITHUB_OUTPUT:?}"
