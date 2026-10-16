#!/usr/bin/env bash
set -euo pipefail
set +x

VERSION=${1:?chart version required}
APP_VERSION=${2:?app version required}
DESTINATION=${3:?destination required}
: "${HELM_SIGNING_KEY_BASE64:?signing key required}"
: "${HELM_SIGNING_KEY_PASSPHRASE:?passphrase required}"
: "${HELM_SIGNING_KEY_FINGERPRINT:?full primary fingerprint required}"
[[ "$HELM_SIGNING_KEY_FINGERPRINT" =~ ^[A-F0-9]{40}$ ]] || { echo 'Expected an uppercase 40-character OpenPGP v4 fingerprint' >&2; exit 1; }

GNUPGHOME=$(mktemp -d)
export GNUPGHOME
cleanup() {
  gpgconf --kill gpg-agent || true
  rm -rf "$GNUPGHOME"
}
trap cleanup EXIT
chmod 700 "$GNUPGHOME"
printf '%s' "$HELM_SIGNING_KEY_BASE64" | base64 --decode | gpg --batch --import
printf '%s' "$HELM_SIGNING_KEY_PASSPHRASE" > "$GNUPGHOME/passphrase"
chmod 600 "$GNUPGHOME/passphrase"

# Export only the configured primary key, never other keys from the supplied bundle.
gpg --batch --with-colons --list-secret-keys "$HELM_SIGNING_KEY_FINGERPRINT" > "$GNUPGHOME/key.txt"
ACTUAL_FINGERPRINT=$(awk -F: '$1 == "fpr" { print $10; exit }' "$GNUPGHOME/key.txt")
[[ "$ACTUAL_FINGERPRINT" == "$HELM_SIGNING_KEY_FINGERPRINT" ]] || { echo 'Signing fingerprint mismatch' >&2; exit 1; }
KEY_NAME=$(python3 - "$GNUPGHOME/key.txt" <<'PY'
import re
import sys
from pathlib import Path
for line in Path(sys.argv[1]).read_text().splitlines():
    fields = line.split(':')
    if fields[0] == 'uid':
        print(re.sub(r'\\x([0-9a-fA-F]{2})', lambda m: chr(int(m[1], 16)), fields[9]))
        break
else:
    sys.exit('Signing key has no UID')
PY
)
gpg --batch --pinentry-mode loopback --passphrase-file "$GNUPGHOME/passphrase" \
  --export-secret-keys "$HELM_SIGNING_KEY_FINGERPRINT" > "$GNUPGHOME/secring.gpg"
gpg --batch --export "$HELM_SIGNING_KEY_FINGERPRINT" > "$GNUPGHOME/pubring.gpg"
mkdir -p "$DESTINATION/keys"
gpg --batch --armor --export "$HELM_SIGNING_KEY_FINGERPRINT" > "$DESTINATION/keys/$HELM_SIGNING_KEY_FINGERPRINT.asc"

for CHART in karpenter karpenter-crd; do
  helm package "charts/$CHART" --version "$VERSION" --app-version "$APP_VERSION" \
    --sign --key "$KEY_NAME" --keyring "$GNUPGHOME/secring.gpg" \
    --passphrase-file "$GNUPGHOME/passphrase" --destination "$DESTINATION"
  helm verify "$DESTINATION/$CHART-$VERSION.tgz" --keyring "$GNUPGHOME/pubring.gpg"
  printf '%s %s\n' "$CHART-$VERSION.tgz" "$HELM_SIGNING_KEY_FINGERPRINT" >> "$DESTINATION/signed-charts.txt"
done
