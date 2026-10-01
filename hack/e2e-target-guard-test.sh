#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cat > "$tmp/gcloud" <<'STUB'
#!/usr/bin/env bash
echo called >> "$CALL_LOG"
exit 1
STUB
chmod +x "$tmp/gcloud"

for operation in setup teardown; do
  log="$tmp/${operation}.log"
  if PATH="$tmp:$PATH" CALL_LOG="$log" E2E_PROJECT_ID=example-project-123 \
      E2E_LOCATION=asia-southeast1-b E2E_PREFIX=tf-e2e-sgp \
      bash "$repo_root/hack/e2e-${operation}.sh" > "$tmp/output" 2>&1; then
    echo "e2e-${operation} allowed a Terraform-owned prefix" >&2
    exit 1
  fi
  if ! grep -q 'Terraform-owned' "$tmp/output"; then
    echo "e2e-${operation} did not explain its rejection" >&2
    exit 1
  fi
  if [[ -e "$log" ]]; then
    echo "e2e-${operation} contacted GCP before the target guard" >&2
    exit 1
  fi
done
