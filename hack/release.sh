#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:?Usage: hack/release.sh <version> (e.g. v0.3.0)}"
[[ "${VERSION}" != v* ]] && VERSION="v${VERSION}"

CHART="charts/karpenter/Chart.yaml"

sed -i.bak "s/^version: .*/version: ${VERSION}/" "${CHART}" && rm "${CHART}.bak"
sed -i.bak "s/^appVersion: .*/appVersion: \"${VERSION}\"/" "${CHART}" && rm "${CHART}.bak"

echo "Bumped ${CHART}:"
grep -E '^(version|appVersion):' "${CHART}"
