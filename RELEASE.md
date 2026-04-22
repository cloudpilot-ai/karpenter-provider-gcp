# Release Process

This document describes how to cut releases of karpenter-provider-gcp.

## Roles

Any of the four project approvers listed in [OWNERS](OWNERS) may cut a release:

- [@patrostkowski](https://github.com/patrostkowski)
- [@jwcesign](https://github.com/jwcesign)
- [@thameezb](https://github.com/thameezb)
- [@dm3ch](https://github.com/dm3ch)

Cherry-pick PRs targeting a release branch require approval from **at least one approver who is not
the author** before merging.

---

## Versioning

The project uses [Semantic Versioning 2.0.0](https://semver.org/).

While the project is at `v0.x.y` (pre-v1):

| Version component | When to bump |
|---|---|
| `y` (patch) | Bug fixes and security patches only — no new features, no breaking changes |
| `x` (minor) | New features **or** breaking changes |
| Major (`v1.0.0`) | TBD — requires explicit team decision |

Release candidates are tagged as `vX.Y.Z-rc.N` (e.g. `v0.3.0-rc.0`) from the release branch
before cutting the stable tag.

---

## Cadence

- **Minor releases** — approximately monthly, driven by accumulated merged features. The release
  manager for that cycle decides when `main` is ready.
- **Patch releases** — on-demand, for critical bug fixes or CVEs. No fixed schedule.
- **Release candidates** — optional, used when a minor release contains significant changes that
  benefit from broader testing before a stable tag.

---

## Release Branches

| Branch name | Convention |
|---|---|
| New branches | `release-v<major>.<minor>.x` (e.g. `release-v0.3.x`) |
| Legacy | `release-0.1`, `release-0.2` — frozen, no new patches |

A release branch is created from `main` at the moment the first `vX.Y.0` tag is pushed. Patch
releases (`vX.Y.1`, `vX.Y.2`, …) are cut from the same `release-vX.Y.x` branch via cherry-pick.

---

## Minor / Major Release

> Requires `gh` CLI and push access to `upstream` (the canonical repo remote).

1. Ensure `main` CI is green.

2. From `main`, run:

   ```bash
   make release VERSION=vX.Y.0
   ```

   The script will:
   - Validate the version string and working-tree cleanliness.
   - Generate a `CHANGELOG.md` entry by extracting `release-note` blocks from PRs merged since the
     previous tag, grouped by `kind/` label.
   - Commit the changelog update.
   - Create and push the `release-vX.Y.x` branch.
   - Create an annotated tag `vX.Y.0` and push it.
   - Open a GitHub Release (draft) with the generated release notes.

3. Verify the `release-build` GitHub Actions workflow completes successfully (image published to
   ECR, Helm chart deployed to GitHub Pages).

4. Publish the draft GitHub Release once you are satisfied with the release notes.

5. Open a follow-up PR on `main` that re-adds the empty `## Unreleased` block to `CHANGELOG.md`.

---

## Patch / Hotfix Release

Fixes must be merged to `main` first (fast-forward), then cherry-picked to the release branch.

> **Exception**: if the fix cannot land on `main` first (e.g. `main` already contains a
> breaking change), discuss with the other approvers before proceeding.

1. Identify the commit SHA(s) on `main` to backport.

2. From any clean checkout, run:

   ```bash
   make cherry-pick BRANCH=release-vX.Y.x COMMITS="<sha1> [<sha2> ...]"
   ```

   The script will check out the release branch, cherry-pick the commits, push a
   `cherry-pick/release-vX.Y.x-<short-sha>` branch, and open a draft PR.

3. Get **at least one other approver** to review and merge the cherry-pick PR.

4. Once merged, check out the release branch locally and run:

   ```bash
   make release VERSION=vX.Y.Z
   ```

---

## Release Candidate

1. From the `release-vX.Y.x` branch, tag manually:

   ```bash
   git tag -a vX.Y.Z-rc.N -m "Release candidate vX.Y.Z-rc.N"
   git push upstream vX.Y.Z-rc.N
   ```

   No CHANGELOG entry is required for RCs; the stable release entry covers them.

2. The `release-build` workflow fires on the `rc` tag and publishes the image and Helm chart with
   the RC tag, so users can test before the stable release.

3. When ready, promote to stable by running `make release VERSION=vX.Y.Z` from the release branch.

---

## Cherry-Pick Policy

Only **bug fixes** and **security patches** are eligible for backporting to a release branch.
New features are never cherry-picked.

A commit is eligible if:
- It is already merged to `main`.
- It targets a currently supported minor release (see [Support Window](#support-window) below).
- It is labelled `kind/bug`, `kind/regression`, or involves a CVE.

---

## Support Window

The project maintains the **most recent minor release** with patch fixes. Older minor releases
receive no further patches once a newer minor is tagged.

| Release | Status |
|---|---|
| Latest minor | Active — receives bug-fix patches |
| Previous minor | End of life once the next minor ships |

---

## Branch Protection

Release branches (`release-v*`) should be protected by org admins with the following ruleset.
If you are cutting the first release on a new branch, **open a GitHub issue or contact an org admin**
to have these rules applied before merging any cherry-pick PRs:

| Rule | Setting |
|---|---|
| Branch name pattern | `release-v*` |
| Require pull request before merging | ✅ — at least 1 approving review |
| Required status checks | `presubmit` (CI job) |
| Require branches to be up to date | ✅ |
| Allow force pushes | ❌ |
| Allow deletions | ❌ |
| Restrict who can push directly | Approvers only: patrostkowski, jwcesign, thameezb, dm3ch |

---

## Post-Release Checklist

- [ ] GitHub Release is published (not draft)
- [ ] `release-build` workflow completed — image and Helm chart are live
- [ ] `## Unreleased` block re-added to `CHANGELOG.md` on `main` (follow-up PR)
- [ ] Helm repo `index.yaml` on GitHub Pages lists the new version (`helm repo update && helm search repo karpenter-gcp`)
- [ ] Previous minor release branch is marked as archived / EOL in this document (if applicable)
