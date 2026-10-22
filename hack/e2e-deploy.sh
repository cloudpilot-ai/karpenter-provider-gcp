#!/usr/bin/env bash
# Shared-target entry point: the trusted coordinator owns the Lease throughout.
set -euo pipefail
REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
cd "${REPO_ROOT}"
exec go run ./hack/e2e-runner local-deploy "$@"
