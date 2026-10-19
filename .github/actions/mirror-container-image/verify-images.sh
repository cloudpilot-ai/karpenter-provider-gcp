#!/usr/bin/env bash
# Verify current anonymous registry access before an installer can mutate a cluster.
set -euo pipefail
umask 077

test_http=false
tags_file=""
refs_file=""
while (($#)); do
  case "$1" in
    --test-http-localhost) test_http=true; shift ;;
    --tags-file) (($# >= 2)) || { echo "Missing tag bindings file." >&2; exit 1; }; tags_file="$2"; shift 2 ;;
    --*) echo "Unsupported verifier option." >&2; exit 1 ;;
    *) [[ -z "$refs_file" ]] || { echo "Expected one image references file." >&2; exit 1; }; refs_file="$1"; shift ;;
  esac
done
[[ -n "$refs_file" && -f "$refs_file" ]] || { echo "Image references file is required." >&2; exit 1; }
[[ -z "$tags_file" || -f "$tags_file" ]] || { echo "Tag bindings file is missing." >&2; exit 1; }
for dependency in curl jq awk wc mktemp; do
  command -v "$dependency" >/dev/null || { echo "Missing verifier dependency: $dependency" >&2; exit 1; }
done
if command -v sha256sum >/dev/null; then hash_command=(sha256sum); else hash_command=(shasum -a 256); fi
command -v "${hash_command[0]}" >/dev/null || { echo "A SHA-256 utility is required." >&2; exit 1; }
temporary="$(mktemp -d "${TMPDIR:-/tmp}/cloudpilot-image-check.XXXXXXXX")"
trap 'rm -rf "$temporary"' EXIT
headers="$temporary/headers"
auth_file="$temporary/authorization"
http_status=""
registry_host=""
registry_repository=""
scheme=https
image_count=0
manifest_count=0
blob_count=0

fail() { echo "Anonymous image preflight failed: $1" >&2; exit 1; }

header_value() {
  awk -v key="$1" '
    {sub(/\r$/, ""); i=index($0, ":")
     if (i && tolower(substr($0, 1, i-1)) == key) {value=substr($0, i+1); sub(/^[ \t]+/, "", value)}}
    END {print value}' "$headers"
}

url_origin() {
  local rest="${1#*://}"
  printf '%s://%s' "${1%%://*}" "${rest%%[/?#]*}"
}

validate_url() {
  local pattern='^(https|http)://([A-Za-z0-9.-]+(:[0-9]+)?)([/?][^[:space:]#]*)?$'
  [[ "$1" =~ $pattern ]] || fail "invalid registry endpoint"
  if [[ "${BASH_REMATCH[1]}" == http ]]; then
    [[ "$test_http" == true && "${BASH_REMATCH[2]}" =~ ^(localhost|127\.0\.0\.1)(:[0-9]+)?$ ]] || fail "HTTPS is required"
  fi
}

http_read() {
  local method="$1" url="$2" output="$3" authorization="$4" limit="$5"
  shift 5
  local location previous_origin redirects=0
  local arguments=()
  while :; do
    validate_url "$url"
    arguments=(--silent --show-error --connect-timeout 10 --max-time 30 --max-redirs 0
      --proto '=https,http' --proto-redir '=https,http' --dump-header "$headers" --output "$output" --write-out '%{http_code}'
      --header 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.artifact.manifest.v1+json')
    if [[ "$method" == HEAD ]]; then arguments+=(--head); else arguments+=(--request GET --max-filesize "$limit"); fi
    [[ -z "$authorization" ]] || arguments+=(--header "@$authorization")
    # An empty environment and --disable prevent curlrc, proxy credentials, and
    # other inherited publisher configuration from proving anonymous access.
    http_status="$(env -i PATH="$PATH" LC_ALL=C curl --disable --no-netrc "${arguments[@]}" "$@" "$url" 2>/dev/null)" || fail "registry network request failed"
    case "$http_status" in
      301|302|303|307|308)
        ((redirects < 5)) || fail "too many registry redirects"
        location="$(header_value location)"
        previous_origin="$(url_origin "$url")"
        case "$location" in
          //*) location="${url%%://*}:$location" ;;
          /*) location="$previous_origin$location" ;;
        esac
        validate_url "$location"
        [[ "$(url_origin "$location")" == "$previous_origin" ]] || authorization=""
        url="$location"
        set --
        redirects=$((redirects + 1)) ;;
      *) return ;;
    esac
  done
}

anonymous_token() {
  local challenge realm service="" scope="repository:$registry_repository:pull" token
  local realm_pattern='(^|[[:space:],])realm="([^"]+)"'
  local service_pattern='(^|[[:space:],])service="([^"]*)"'
  local scope_pattern='(^|[[:space:],])scope="([^"]*)"'
  challenge="$(header_value www-authenticate)"
  [[ "$challenge" =~ ^[Bb][Ee][Aa][Rr][Ee][Rr][[:space:]] && "$challenge" != *\\* ]] || fail "registry requires unsupported authentication"
  [[ "$challenge" =~ $realm_pattern ]] || fail "registry Bearer realm is missing"
  realm="${BASH_REMATCH[2]}"
  if [[ "$challenge" =~ $service_pattern ]]; then service="${BASH_REMATCH[2]}"; fi
  if [[ "$challenge" =~ $scope_pattern ]]; then scope="${BASH_REMATCH[2]}"; fi
  http_read GET "$realm" "$temporary/token.json" "" 1048576 --get --data-urlencode "service=$service" --data-urlencode "scope=$scope"
  [[ "$http_status" == 200 ]] || fail "anonymous registry token was denied"
  token="$(jq -er '.token // .access_token | select(type == "string" and length > 0 and length <= 65536 and test("^[A-Za-z0-9._~+/-]+=*$"))' "$temporary/token.json" 2>/dev/null)" || fail "invalid anonymous registry token"
  printf 'Authorization: Bearer %s\n' "$token" > "$auth_file"
  unset token
}

registry_read() {
  local method="$1" kind="$2" identifier="$3" output="$4" authorization=""
  [[ ! -f "$auth_file" ]] || authorization="$auth_file"
  http_read "$method" "$scheme://$registry_host/v2/$registry_repository/$kind/$identifier" "$output" "$authorization" 16777216
  if [[ "$http_status" == 401 ]]; then
    anonymous_token
    http_read "$method" "$scheme://$registry_host/v2/$registry_repository/$kind/$identifier" "$output" "$auth_file" 16777216
  fi
  [[ "$http_status" == 200 ]] || fail "required $kind object is missing or unauthorized"
}

select_repository() {
  local pattern='^([a-z0-9.-]+(:[0-9]+)?)/([a-z0-9]+(([._-]|/)[a-z0-9]+)*)$'
  [[ "$1" =~ $pattern ]] || fail "invalid explicit registry/repository"
  registry_host="${BASH_REMATCH[1]}"
  registry_repository="${BASH_REMATCH[3]}"
  [[ "$registry_host" == *.* || "$registry_host" == *:* || "$registry_host" == localhost ]] || fail "an explicit registry host is required"
  scheme=https
  if [[ "$test_http" == true && "$registry_host" =~ ^(localhost|127\.0\.0\.1)(:[0-9]+)?$ ]]; then scheme=http; fi
  : > "$auth_file"
  # An absent header is different from an empty Authorization header to registries.
  rm -f "$auth_file"
}

validate_manifest() {
  jq -e '
    def descriptor:
      type == "object" and (.digest | type == "string" and test("^sha256:[0-9a-f]{64}$"))
      and (.size | type == "number" and . >= 0 and . <= 9007199254740991 and floor == .)
      and (.mediaType | type == "string" and length > 0)
      and ((.urls // []) | type == "array" and length == 0);
    def descriptors: type == "array" and all(.[]; descriptor);
    type == "object" and (
      if .mediaType == "application/vnd.oci.image.index.v1+json" or .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
      then .schemaVersion == 2 and (.manifests | descriptors and length > 0)
      elif .mediaType == "application/vnd.oci.image.manifest.v1+json" or .mediaType == "application/vnd.docker.distribution.manifest.v2+json"
      then .schemaVersion == 2 and (.config | descriptor) and (.layers | descriptors)
      elif .mediaType == "application/vnd.oci.artifact.manifest.v1+json"
      then (.artifactType | type == "string" and length > 0) and (.blobs | descriptors)
      else false end)
    and (if has("subject") then .subject | descriptor else true end)
  ' "$1" >/dev/null 2>&1 || fail "unsupported manifest schema or invalid descriptor"
}

verify_manifest_file() {
  local file="$1" expected="$2" expected_size="$3" actual
  actual="$("${hash_command[@]}" "$file")"
  [[ "sha256:${actual%% *}" == "$expected" ]] || fail "manifest SHA-256 mismatch"
  if [[ "$expected_size" != - ]]; then
    actual="$(wc -c < "$file")"
    [[ "${actual//[[:space:]]/}" == "$expected_size" ]] || fail "manifest size mismatch"
  fi
  validate_manifest "$file"
}

verify_blob() {
  local digest="$1" expected_size="$2" marker="$3" length returned_digest
  if [[ -f "$marker" ]]; then
    [[ "$(cat "$marker")" == "$expected_size" ]] || fail "conflicting blob descriptor sizes"
    return
  fi
  blob_count=$((blob_count + 1))
  ((blob_count <= 65536)) || fail "image blob graph exceeds the limit"
  registry_read HEAD blobs "$digest" "$temporary/blob-head"
  length="$(header_value content-length)"
  [[ "$length" == "$expected_size" ]] || fail "blob size mismatch or missing Content-Length"
  returned_digest="$(header_value docker-content-digest)"
  [[ -z "$returned_digest" || "$returned_digest" == "$digest" ]] || fail "blob digest header mismatch"
  printf '%s\n' "$expected_size" > "$marker"
}

verify_image() {
  local ref="$1" base digest expected_size file child size count=0
  [[ "$ref" =~ ^[^@]+@sha256:[0-9a-f]{64}$ ]] || fail "expected a full repository@sha256 reference"
  base="${ref%@*}"; digest="${ref##*@}"
  select_repository "$base"
  local graph="$temporary/image-$image_count"
  mkdir "$graph"
  printf '%s\t-\n' "$digest" > "$graph/queue"
  # Children are intentionally appended before the next read from the open queue.
  # shellcheck disable=SC2094
  while IFS=$'\t' read -r digest expected_size; do
    count=$((count + 1)); ((count <= 16384)) || fail "image descriptor graph exceeds the limit"
    file="$graph/${digest#sha256:}.json"
    if [[ -f "$file" ]]; then verify_manifest_file "$file" "$digest" "$expected_size"; continue; fi
    manifest_count=$((manifest_count + 1)); ((manifest_count <= 4096)) || fail "image manifest graph exceeds the limit"
    registry_read GET manifests "$digest" "$file"
    verify_manifest_file "$file" "$digest" "$expected_size"
    jq -r '.manifests[]? | [.digest, .size] | @tsv' "$file" >> "$graph/queue"
    jq -r '(.config // empty), .layers[]?, .blobs[]? | [.digest, .size] | @tsv' "$file" > "$graph/blobs"
    while IFS=$'\t' read -r child size; do
      verify_blob "$child" "$size" "$graph/blob-${child#sha256:}"
    done < "$graph/blobs"
  done < "$graph/queue"
}

while IFS= read -r ref || [[ -n "$ref" ]]; do
  [[ -n "$ref" ]] || continue
  image_count=$((image_count + 1)); ((image_count <= 256)) || fail "too many image references"
  verify_image "$ref"
done < "$refs_file"
((image_count > 0)) || fail "the image references file is empty"

if [[ -n "$tags_file" ]]; then
  tag_count=0
  while IFS=$'\t' read -r binding expected extra || [[ -n "$binding" ]]; do
    [[ -n "$binding" ]] || continue
    tag_count=$((tag_count + 1)); ((tag_count <= 256)) || fail "too many tag bindings"
    [[ -z "$extra" && "$expected" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "invalid tag binding digest"
    tag="${binding##*:}"; base="${binding%:*}"
    [[ "$tag" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ && "$base" != "$binding" ]] || fail "invalid tag binding"
    select_repository "$base"
    registry_read GET manifests "$tag" "$temporary/tag.json"
    verify_manifest_file "$temporary/tag.json" "$expected" -
  done < "$tags_file"
fi
printf 'Verified %s image references anonymously.\n' "$image_count"
