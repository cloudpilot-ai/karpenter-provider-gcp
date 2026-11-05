#!/usr/bin/env bash
# Copyright The Kubernetes Authors.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

case "${1:-}" in
  '') DRY_RUN=false ;;
  --dry-run) DRY_RUN=true ;;
  *) echo "Usage: $0 [--dry-run]" >&2; exit 2 ;;
esac

cd "$(dirname "${BASH_SOURCE[0]}")/.."
ROOT=$PWD
MODULES=(. hack/tools hack/crd-tools hack/pdcsi-data)
BASE=cgr.dev/chainguard/static
TEMP=$(mktemp -d)
trap 'rm -rf "$TEMP"' EXIT
export GOWORK=off

fail() { echo "$*" >&2; exit 1; }

CURRENT=$(awk '$1 == "go" {print $2}' go.mod)
[[ "$CURRENT" =~ ^1\.[0-9]+\.[0-9]+$ ]] || fail 'Expected an exact Go patch in go.mod'
MINOR=${CURRENT%.*}
# Resolve outside the repository; don't load or change project dependencies.
VERSION=$(cd "$TEMP" && go list -mod=mod -m -f '{{.Version}}' "go@$MINOR")
[[ "$VERSION" =~ ^1\.[0-9]+\.[0-9]+$ && "${VERSION%.*}" == "$MINOR" ]] || fail 'Invalid resolved Go patch'

# Validate every input before modifying any pin.
for module in "${MODULES[@]}"; do
  CURRENT=$(awk '$1 == "go" {print $2}' "$module/go.mod")
  [[ "$CURRENT" =~ ^1\.[0-9]+\.[0-9]+$ && "${CURRENT%.*}" == "$MINOR" ]] || fail "Go minor drift in $module"
  (( 10#${VERSION##*.} >= 10#${CURRENT##*.} )) || fail "Refusing Go downgrade in $module"
  if grep -q '^toolchain ' "$module/go.mod"; then
    fail "Review separate toolchain pin in $module manually"
  fi
done
[[ $(grep -Ec '^defaultBaseImage: cgr\.dev/chainguard/static:latest@sha256:[a-f0-9]{64}$' .ko.yaml) == 1 ]] || fail 'Unexpected ko base configuration'
DIGEST=$(crane digest "$BASE:latest")
[[ "$DIGEST" =~ ^sha256:[a-f0-9]{64}$ ]] || fail 'Invalid base digest'
# Inspect the resolved index, not a tag that can move between requests.
crane manifest "$BASE@$DIGEST" | jq -e '
  [.manifests[].platform | "\(.os)/\(.architecture)"] |
  contains(["linux/amd64", "linux/arm64"])' >/dev/null

echo "Go $VERSION; base $BASE@$DIGEST"
if [[ "$DRY_RUN" == true ]]; then
  echo 'Dry run: no files changed'
  exit 0
fi
for module in "${MODULES[@]}"; do
  (cd "$ROOT/$module" && go mod edit -go="$VERSION")
done
awk -v image="$BASE:latest@$DIGEST" '
  $1 == "defaultBaseImage:" {$0 = "defaultBaseImage: " image} {print}
' .ko.yaml > "$TEMP/ko.yaml"
cp "$TEMP/ko.yaml" .ko.yaml
