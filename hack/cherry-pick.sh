#!/usr/bin/env bash
# Usage: ./hack/cherry-pick.sh <release-branch> <commit> [<commit> ...]
#   e.g. ./hack/cherry-pick.sh release-v0.3.x abc1234 def5678
#
# Checks out the release branch, cherry-picks the given commits onto a new
# branch, pushes it, and opens a draft PR for review.
#
# Requires: git, gh (GitHub CLI)

set -euo pipefail

RELEASE_BRANCH="${1:-}"
shift || true
COMMITS=("$@")

# ── helpers ───────────────────────────────────────────────────────────────────

die()  { echo "ERROR: $*" >&2; exit 1; }
info() { echo "==> $*"; }

# ── validation ────────────────────────────────────────────────────────────────

[[ -n "${RELEASE_BRANCH}" ]]   || die "Usage: $0 <release-branch> <commit> [<commit> ...]"
[[ "${#COMMITS[@]}" -gt 0 ]]   || die "At least one commit SHA is required."

if ! [[ "${RELEASE_BRANCH}" =~ ^release-v[0-9]+\.[0-9]+\.x$ ]]; then
  die "Release branch must match 'release-vX.Y.x'. Got: '${RELEASE_BRANCH}'"
fi

command -v gh &>/dev/null || die "'gh' CLI is required. Install from https://cli.github.com/"

# ── stash any uncommitted work ────────────────────────────────────────────────

STASH_NEEDED=false
if ! git diff --quiet || ! git diff --cached --quiet; then
  info "Stashing uncommitted changes..."
  git stash push -m "cherry-pick.sh auto-stash"
  STASH_NEEDED=true
fi

# restore stash on exit (best-effort)
cleanup() {
  if [[ "${STASH_NEEDED}" == "true" ]]; then
    info "Restoring stashed changes..."
    git stash pop || echo "WARNING: Could not pop stash automatically. Run 'git stash pop' manually."
  fi
}
trap cleanup EXIT

ORIGINAL_BRANCH="$(git branch --show-current)"

# ── fetch and check out release branch ───────────────────────────────────────

info "Fetching upstream..."
git fetch upstream

if ! git branch --list "${RELEASE_BRANCH}" | grep -q .; then
  info "Checking out '${RELEASE_BRANCH}' from upstream..."
  git checkout -b "${RELEASE_BRANCH}" "upstream/${RELEASE_BRANCH}"
else
  info "Updating local '${RELEASE_BRANCH}'..."
  git checkout "${RELEASE_BRANCH}"
  git merge --ff-only "upstream/${RELEASE_BRANCH}"
fi

# ── create cherry-pick branch ─────────────────────────────────────────────────

SHORT_SHA="$(git rev-parse --short "${COMMITS[0]}")"
CP_BRANCH="cherry-pick/${RELEASE_BRANCH}-${SHORT_SHA}"

info "Creating branch '${CP_BRANCH}'..."
git checkout -b "${CP_BRANCH}"

# ── cherry-pick ───────────────────────────────────────────────────────────────

for COMMIT in "${COMMITS[@]}"; do
  info "Cherry-picking ${COMMIT}..."
  # -x appends "(cherry picked from commit <sha>)" to the commit message
  git cherry-pick -x "${COMMIT}"
done

# ── push ─────────────────────────────────────────────────────────────────────

info "Pushing '${CP_BRANCH}'..."
git push upstream "${CP_BRANCH}"

# ── open draft PR ─────────────────────────────────────────────────────────────

PR_TITLE="cherry-pick: backport to ${RELEASE_BRANCH}"
PR_BODY="$(cat <<EOF
#### What type of PR is this?

/kind bug

#### What this PR does / why we need it:

Cherry-pick of the following commit(s) from \`main\` onto \`${RELEASE_BRANCH}\`:

$(for C in "${COMMITS[@]}"; do echo "- $(git log -1 --oneline "${C}")"; done)

#### Which issue(s) this PR fixes:

Fixes #

#### Docs and examples

- [x] \`docs/\` and \`examples/\` updated (or this PR does not affect user-facing behaviour, configuration, or APIs)

#### Special notes for your reviewer:

Please verify this cherry-pick applies cleanly and the fix is appropriate for a patch release.

#### Does this PR introduce a user-facing change?

\`\`\`release-note
NONE
\`\`\`
EOF
)"

info "Opening draft PR against '${RELEASE_BRANCH}'..."
PR_URL="$(gh pr create \
  --base "${RELEASE_BRANCH}" \
  --head "${CP_BRANCH}" \
  --title "${PR_TITLE}" \
  --body "${PR_BODY}" \
  --draft)"

info ""
info "Draft PR opened: ${PR_URL}"
info ""
info "Next steps:"
info "  1. Update the PR description (title, issue link, release-note block)."
info "  2. Get at least one other approver to review and merge."
info "  3. After merge, run: make release VERSION=vX.Y.Z"

# return to original branch (cleanup trap handles stash pop)
git checkout "${ORIGINAL_BRANCH}"
