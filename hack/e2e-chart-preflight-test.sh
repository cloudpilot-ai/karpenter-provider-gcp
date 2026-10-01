#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cat > "$tmp/helm" <<'STUB'
#!/usr/bin/env bash
echo "helm $*" >> "$CALL_LOG"
if [[ "$1" == dependency && "$2" == build ]]; then exit 79; fi
exit 0
STUB
cat > "$tmp/gcloud" <<'STUB'
#!/usr/bin/env bash
echo "gcloud $*" >> "$CALL_LOG"
exit 0
STUB
cat > "$tmp/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "kubectl $*" >> "$CALL_LOG"
exit 0
STUB
chmod +x "$tmp/helm" "$tmp/gcloud" "$tmp/kubectl"

for operation in setup deploy; do
  log="$tmp/${operation}.log"
  if PATH="$tmp:$PATH" CALL_LOG="$log" E2E_PROJECT_ID=example-project-123 \
      E2E_LOCATION=asia-southeast1-b E2E_REGION=asia-southeast1 E2E_PREFIX=local-e2e \
      bash "$repo_root/hack/e2e-${operation}.sh" > "$tmp/output" 2>&1; then
    echo "e2e-${operation} ignored chart preflight failure" >&2
    exit 1
  fi
  if ! grep -q '^helm dependency build ' "$log"; then
    echo "e2e-${operation} did not preflight pinned chart dependency" >&2
    exit 1
  fi
  if grep -Eq '^(gcloud|kubectl|helm upgrade) ' "$log"; then
    echo "e2e-${operation} mutated/reached target before chart preflight" >&2
    exit 1
  fi
done
