#!/usr/bin/env bash
set -euo pipefail

# Keep these release asset hashes fixed; never trust a checksum downloaded beside a binary.
version=v0.11.6
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo 'Unsupported registry-client architecture.' >&2; exit 2 ;;
esac
case "${os}-${arch}" in
  linux-amd64) checksum=8e0e62a497fcdb8048d18aa927a139613176ba0531f412bc541044e28f9856bd ;;
  linux-arm64) checksum=a9b71a3ee79b2d1dbbd7d51fd5e8fa214722c192864235d3d8764463c751a1ff ;;
  darwin-amd64) checksum=d651381c58357e9b9446cec172761c7418b7621da57960d438175d8f4c5b479d ;;
  darwin-arm64) checksum=5b52b71861abc300ab513e87a4c3794cff6508035ac7f6b6bdfa1fa5688c0040 ;;
  *) echo 'Unsupported registry-client operating system.' >&2; exit 2 ;;
esac
destination="${1:?Usage: install-regctl.sh OUTPUT_DIRECTORY}"
mkdir -p "$destination"
if [[ -f "$destination/regctl" ]]; then
  actual="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$destination/regctl")"
  if [[ "$actual" == "$checksum" ]]; then exit 0; fi
fi
download="$(mktemp -d "${destination}/download.XXXXXX")"
trap 'rm -rf "$download"' EXIT
asset="regctl-${os}-${arch}"
gh release download "$version" --repo regclient/regclient --pattern "$asset" --dir "$download"
actual="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$download/$asset")"
if [[ "$actual" != "$checksum" ]]; then
  echo 'Registry-client checksum mismatch.' >&2
  exit 1
fi
chmod 755 "$download/$asset"
mv "$download/$asset" "$destination/regctl"
