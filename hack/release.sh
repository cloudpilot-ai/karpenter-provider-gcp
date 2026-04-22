#!/usr/bin/env bash
# Usage: ./hack/release.sh <version>
#   e.g. ./hack/release.sh v0.3.0
#        ./hack/release.sh v0.3.0-rc.0
#
# Requires: git, gh (GitHub CLI), jq

set -euo pipefail

VERSION="${1:-}"

# ── helpers ──────────────────────────────────────────────────────────────────

die() { echo "ERROR: $*" >&2; exit 1; }
info() { echo "==> $*"; }

# ── validation ────────────────────────────────────────────────────────────────

[[ -n "${VERSION}" ]] || die "Usage: $0 <version>  (e.g. v0.3.0)"

# Accept vX.Y.Z or vX.Y.Z-rc.N
if ! [[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
  die "Version '${VERSION}' is not valid semver. Expected vX.Y.Z or vX.Y.Z-rc.N"
fi

IS_RC=false
[[ "${VERSION}" =~ -rc\. ]] && IS_RC=true

# Working tree must be clean
if ! git diff --quiet || ! git diff --cached --quiet; then
  die "Working tree is not clean. Commit or stash your changes first."
fi

CURRENT_BRANCH="$(git branch --show-current)"

if [[ "${IS_RC}" == "false" ]]; then
  # Determine if this is a minor/major (from main) or patch (from release branch)
  if [[ "${CURRENT_BRANCH}" == "main" ]]; then
    RELEASE_TYPE="minor"
  elif [[ "${CURRENT_BRANCH}" =~ ^release-v[0-9]+\.[0-9]+\.x$ ]]; then
    RELEASE_TYPE="patch"
  else
    die "Stable releases must be run from 'main' (minor/major) or 'release-vX.Y.x' (patch). Current branch: '${CURRENT_BRANCH}'"
  fi
fi

# Tag must not already exist
if git rev-parse "${VERSION}" &>/dev/null; then
  die "Tag '${VERSION}' already exists."
fi

command -v gh  &>/dev/null || die "'gh' CLI is required. Install from https://cli.github.com/"
command -v jq  &>/dev/null || die "'jq' is required."

info "Releasing ${VERSION} (type: ${IS_RC:+RC}${IS_RC:-${RELEASE_TYPE:-rc}})"

# ── derive version components ─────────────────────────────────────────────────

SEMVER="${VERSION#v}"                            # strip leading v
MAJOR="$(echo "${SEMVER}" | cut -d. -f1)"
MINOR="$(echo "${SEMVER}" | cut -d. -f2)"
RELEASE_BRANCH="release-v${MAJOR}.${MINOR}.x"

# ── find previous tag (for CHANGELOG and PR range) ───────────────────────────

PREV_TAG="$(git tag --sort=-version:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)"
[[ -n "${PREV_TAG}" ]] || die "No previous stable tag found. Cannot determine PR range."
info "Previous stable tag: ${PREV_TAG}"

# ── generate CHANGELOG section (stable releases only) ────────────────────────

if [[ "${IS_RC}" == "false" ]]; then
  info "Generating CHANGELOG section from merged PRs since ${PREV_TAG}..."

  # Fetch merged PRs since the previous tag date
  PREV_TAG_DATE="$(git log -1 --format=%aI "${PREV_TAG}")"

  CHANGELOG_SECTION="$(
    gh pr list \
      --base main \
      --state merged \
      --json number,title,labels,body \
      --limit 500 \
      --jq "
        [
          .[] |
          select(.mergedAt >= \"${PREV_TAG_DATE}\" // true) |
          {
            number: .number,
            title:  .title,
            labels: [.labels[].name],
            note:   (
              .body |
              capture(\"[\\\\x60]{3}release-note\\n(?P<note>[\\\\s\\\\S]+?)\\n[\\\\x60]{3}\") |
              .note // empty
            )
          } |
          select(.note != null and (.note | ascii_downcase | ltrimstr(\" \") | ltrimstr(\"\\n\") | startswith(\"none\") | not))
        ]
      " 2>/dev/null
  )"

  # Build section text via jq
  SECTION_TEXT="$(echo "${CHANGELOG_SECTION}" | jq -r --arg version "${VERSION}" '
    def label_section(lbl):
      map(select(.labels | index(lbl))) |
      if length > 0 then
        ["### " + (
          if lbl == "kind/api-change" or lbl == "kind/deprecation" then "Breaking Changes"
          elif lbl == "kind/feature" then "New Features"
          elif lbl == "kind/bug" or lbl == "kind/regression" then "Bug Fixes"
          elif lbl == "kind/cleanup" or lbl == "kind/chore" then "Maintenance"
          elif lbl == "kind/documentation" then "Documentation"
          else "Other Changes" end
        )] + map("- \(.note | gsub("\n";" ")) (#\(.number))")
      else [] end;

    (
      label_section("kind/api-change") +
      label_section("kind/deprecation") +
      label_section("kind/feature") +
      label_section("kind/bug") +
      label_section("kind/regression") +
      label_section("kind/cleanup") +
      label_section("kind/chore") +
      label_section("kind/documentation") +
      (
        map(select(
          (.labels | (index("kind/api-change") == null)) and
          (.labels | (index("kind/deprecation") == null)) and
          (.labels | (index("kind/feature") == null)) and
          (.labels | (index("kind/bug") == null)) and
          (.labels | (index("kind/regression") == null)) and
          (.labels | (index("kind/cleanup") == null)) and
          (.labels | (index("kind/chore") == null)) and
          (.labels | (index("kind/documentation") == null))
        )) |
        if length > 0 then
          ["### Other Changes"] + map("- \(.note | gsub("\n";" ")) (#\(.number))")
        else [] end
      )
    ) | join("\n")
  ')"

  if [[ -z "${SECTION_TEXT}" ]]; then
    SECTION_TEXT="No user-facing changes."
  fi

  NEW_SECTION="## ${VERSION}"$'\n\n'"${SECTION_TEXT}"

  # Prepend new section after the header line(s) in CHANGELOG.md
  # The header block is everything up to and including the blank line after "## Unreleased" or the first "## v"
  CHANGELOG="CHANGELOG.md"
  [[ -f "${CHANGELOG}" ]] || die "CHANGELOG.md not found in $(pwd)"

  # Insert new section: after the first blank line that follows the file header (before any existing ## section)
  HEADER_END="$(grep -n '^## ' "${CHANGELOG}" | head -1 | cut -d: -f1)"
  if [[ -n "${HEADER_END}" ]]; then
    # Insert before the first existing ## section
    INSERT_AT="$((HEADER_END - 1))"
    {
      head -n "${INSERT_AT}" "${CHANGELOG}"
      echo ""
      echo "${NEW_SECTION}"
      echo ""
      tail -n +"$((INSERT_AT + 1))" "${CHANGELOG}"
    } > "${CHANGELOG}.tmp"
    mv "${CHANGELOG}.tmp" "${CHANGELOG}"
  else
    # No sections yet — append
    printf '\n%s\n' "${NEW_SECTION}" >> "${CHANGELOG}"
  fi

  info "Committing CHANGELOG.md..."
  git add CHANGELOG.md
  git commit -m "chore: finalize CHANGELOG for ${VERSION}"
fi

# ── create release branch (minor/major only) ──────────────────────────────────

if [[ "${IS_RC}" == "false" && "${RELEASE_TYPE}" == "minor" ]]; then
  if git branch --list "${RELEASE_BRANCH}" | grep -q .; then
    info "Release branch '${RELEASE_BRANCH}' already exists locally — skipping creation."
  else
    info "Creating release branch ${RELEASE_BRANCH}..."
    git checkout -b "${RELEASE_BRANCH}"
    git push upstream "${RELEASE_BRANCH}"
    git checkout main
    info "Switched back to main."
  fi
fi

# ── tag ───────────────────────────────────────────────────────────────────────

info "Creating annotated tag ${VERSION}..."
git tag -a "${VERSION}" -m "Release ${VERSION}"

info "Pushing tag ${VERSION}..."
git push upstream "${VERSION}"

# ── GitHub Release ────────────────────────────────────────────────────────────

info "Opening GitHub Release (draft)..."

if [[ "${IS_RC}" == "true" ]]; then
  RELEASE_NOTES="Release candidate ${VERSION}. See the upcoming stable release for the full changelog."
  RELEASE_FLAGS="--prerelease --draft"
else
  # Extract the section we just wrote from CHANGELOG.md as the release body
  RELEASE_NOTES="$(awk "/^## ${VERSION}/{found=1; next} found && /^## /{exit} found{print}" CHANGELOG.md | sed '/^[[:space:]]*$/d;1s/^//' | head -200)"
  RELEASE_FLAGS="--draft"
fi

# shellcheck disable=SC2086
gh release create "${VERSION}" \
  --title "Release ${VERSION}" \
  --notes "${RELEASE_NOTES}" \
  ${RELEASE_FLAGS}

info "Done! The GitHub Release is open as a draft."
info "Next steps:"
info "  1. Wait for the 'release-build' CI workflow to complete."
info "  2. Review and publish the draft GitHub Release."
if [[ "${IS_RC}" == "false" && "${RELEASE_TYPE}" == "minor" ]]; then
  info "  3. Open a follow-up PR on main re-adding the '## Unreleased' block to CHANGELOG.md."
fi
