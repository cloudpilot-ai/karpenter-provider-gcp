#!/usr/bin/env bash
# Copyright The Kubernetes Authors.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

SOURCE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TEMP=$(mktemp -d)
trap 'rm -rf "$TEMP"' EXIT
ROOT=$TEMP/repo
mkdir -p "$TEMP/bin" "$ROOT/hack/tools" "$ROOT/hack/crd-tools" "$ROOT/hack/pdcsi-data"
cp "$SOURCE/update-security-pins.sh" "$ROOT/hack/"
for module in . hack/tools hack/crd-tools hack/pdcsi-data; do
  printf 'module example.test/project\n\ngo 1.27.3\n\nrequire example.test/dep v1.2.3\n' > "$ROOT/$module/go.mod"
done
OLD=sha256:$(printf '%064d' 0)
NEW=sha256:$(printf '%064d' 1)
printf 'defaultBaseImage: cgr.dev/chainguard/static:latest@%s\ndefaultPlatforms:\n  - linux/arm64\n  - linux/amd64\n' "$OLD" > "$ROOT/.ko.yaml"
echo unchanged > "$ROOT/unrelated"

# CLI doubles keep these regression tests deterministic and offline.
cat > "$TEMP/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == list ]]; then
  [[ ${GO_FAIL:-false} == false ]]
  echo "${GO_PATCH:-1.27.10}"
else
  [[ $1 == mod && $2 == edit ]]
  sed "s/^go .*/go ${3#-go=}/" go.mod > go.mod.new
  mv go.mod.new go.mod
fi
EOF
cat > "$TEMP/bin/crane" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == digest ]]; then
  echo "$BASE_DIGEST"
else
  [[ $1 == manifest && $2 == "cgr.dev/chainguard/static@$BASE_DIGEST" ]]
  if [[ ${SINGLE_ARCH:-false} == true ]]; then
    echo '{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}}]}'
  else
    echo '{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}},{"platform":{"os":"linux","architecture":"arm64"}}]}'
  fi
fi
EOF
chmod +x "$TEMP/bin/go" "$TEMP/bin/crane"
export PATH="$TEMP/bin:$PATH" BASE_DIGEST="$NEW"
cd "$ROOT"

snapshot() { cksum go.mod hack/{tools,crd-tools,pdcsi-data}/go.mod .ko.yaml unrelated; }
run_update() { bash hack/update-security-pins.sh "$@"; }
expect_failure() {
  snapshot > "$TEMP/before"
  if run_update > "$TEMP/output" 2>&1; then
    echo 'Expected invalid update to fail' >&2; exit 1
  fi
  snapshot > "$TEMP/after"
  diff -u "$TEMP/before" "$TEMP/after"
}

snapshot > "$TEMP/before"
run_update --dry-run
snapshot > "$TEMP/after"
diff -u "$TEMP/before" "$TEMP/after"
for patch in 1.28.0 1.27.2 1.27.11rc1 '1.27.4; echo unsafe'; do
  export GO_PATCH=$patch
  expect_failure
done
unset GO_PATCH
export GO_FAIL=true; expect_failure; unset GO_FAIL
export BASE_DIGEST=invalid; expect_failure; export BASE_DIGEST=$NEW
export SINGLE_ARCH=true; expect_failure; unset SINGLE_ARCH
printf '\ntoolchain go1.27.4\n' >> hack/tools/go.mod
expect_failure
sed '/^toolchain /d' hack/tools/go.mod > "$TEMP/module"
cp "$TEMP/module" hack/tools/go.mod

run_update
for module in . hack/tools hack/crd-tools hack/pdcsi-data; do
  grep -qx 'go 1.27.10' "$module/go.mod"
  grep -qx 'require example.test/dep v1.2.3' "$module/go.mod"
done
grep -qx "defaultBaseImage: cgr.dev/chainguard/static:latest@$NEW" .ko.yaml
grep -qx '  - linux/arm64' .ko.yaml
grep -qx '  - linux/amd64' .ko.yaml
grep -qx unchanged unrelated
snapshot > "$TEMP/before"
run_update
snapshot > "$TEMP/after"
diff -u "$TEMP/before" "$TEMP/after"
echo 'Security pin updater tests passed'
