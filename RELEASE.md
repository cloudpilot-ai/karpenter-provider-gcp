# Release Process

## Cadence

- **Minor versions** (`vX.Y.0`): released monthly, targeting the first week, when there are changes to ship.
- **Patch versions** (`vX.Y.Z`): released as needed for critical bug fixes, cherry-picked onto the active release branch.

## Code Freeze

One week before a minor release:

- No new features are reviewed or merged.
- Only release-critical bug fixes and stabilization PRs are accepted.

---

## Minor Release

### 1. Verify readiness

```bash
git fetch origin && git checkout main && git reset --hard origin/main
```

- Confirm `maxKubernetesVersion` in the codebase reflects the latest supported GKE version.

### 2. Update release artefacts and run e2e

Edit `MIGRATION.md`:

- Move items from the `Unreleased` section into a new `vX.Y.0` section.
- Leave an empty `Unreleased` section at the top.

Open a _"chore: prepare vX.Y.0 release"_ PR and add the `release-prep` label (excludes it from the auto-generated changelog). Then run the full e2e suite — the PR only touches docs, so this validates the release candidate code before the merge commit is tagged:

```bash
make e2e-deploy E2E_PROJECT_ID=<PROJECT_ID> E2E_SA_PATH=<SA_KEY> E2E_LOCATION=<ZONE> E2E_REGION=<REGION>
make e2e-tests  E2E_PROJECT_ID=<PROJECT_ID> E2E_SA_PATH=<SA_KEY> E2E_LOCATION=<ZONE> E2E_REGION=<REGION>
```

All specs must pass before merging. Once green, get the PR reviewed and merged into `main`.

### 3. Create release branch and tag

After the release PR is merged, fetch latest `main` and create the branch and tag:

```bash
git fetch origin && git checkout main && git reset --hard origin/main

# Create and push the release branch (pattern: release-X.Y, never release-X.Y.Z)
git checkout -b release-X.Y
git push https://github.com/cloudpilot-ai/karpenter-provider-gcp.git release-X.Y

# Capture the exact commit SHA — never rely on HEAD when tagging
COMMIT=$(git rev-parse origin/release-X.Y)
echo "Tagging commit: $COMMIT"

# Create a signed tag on the explicit SHA
git tag -s vX.Y.0 "$COMMIT" -m "Release vX.Y.0"

# Verify the tag object points to the right commit before pushing
git cat-file -p vX.Y.0 | head -3

git push https://github.com/cloudpilot-ai/karpenter-provider-gcp.git vX.Y.0
```

### 4. Create GitHub release

```bash
# Create a draft with auto-generated changelog and migration note prepended.
# --notes and --generate-notes are mutually exclusive, so generate the body
# via the API in a subshell and pass the combined text directly.
gh release create vX.Y.0 \
  --repo cloudpilot-ai/karpenter-provider-gcp \
  --title "vX.Y.0" \
  --notes "For the full migration guide see [MIGRATION.md](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/vX.Y.0/MIGRATION.md).

$(gh api repos/cloudpilot-ai/karpenter-provider-gcp/releases/generate-notes \
    --method POST \
    --field tag_name="vX.Y.0" \
    --field previous_tag_name="vA.B.C" \
    --field target_commitish="main" \
    --jq '.body')" \
  --draft
```

> `.github/release.yml` controls changelog categories and excludes `release-prep`-labelled PRs automatically.

Review the draft at `https://github.com/cloudpilot-ai/karpenter-provider-gcp/releases`, then publish:

```bash
gh release edit vX.Y.0 --repo cloudpilot-ai/karpenter-provider-gcp --draft=false
```

### 5. Verify published artefacts

```bash
# Docker image (ECR Public)
curl -s "https://public.ecr.aws/v2/cloudpilotai/gcp/karpenter/tags/list"

# Helm charts (GitHub Pages)
curl -s "https://cloudpilot-ai.github.io/karpenter-provider-gcp/index.yaml" \
  | grep -A3 "version: X.Y.0"
```

Also confirm the new version appears on [ArtifactHub](https://artifacthub.io/packages/helm/karpenter-provider-gcp/karpenter).

### 6. Validate on the e2e cluster

Install the published chart and run e2e from the release branch (tests must match the released code):

```bash
git checkout release-X.Y

make e2e-deploy \
  RELEASE_VERSION=X.Y.0 \
  E2E_PROJECT_ID=<PROJECT_ID> \
  E2E_SA_PATH=<SA_KEY> \
  E2E_LOCATION=<ZONE>

make e2e-tests \
  E2E_PROJECT_ID=<PROJECT_ID> \
  E2E_SA_PATH=<SA_KEY> \
  E2E_LOCATION=<ZONE> \
  E2E_REGION=<REGION>
```

`RELEASE_VERSION` switches `make e2e-deploy` into published-chart mode — it installs from the Helm repo at the given version with no local image build. All specs must pass before announcing.

### 7. Announce

Post a release announcement to **#karpenter-gcp** on [Kubernetes Slack](https://kubernetes.slack.com).

---

## Patch Release

1. Create a branch off the relevant release branch and cherry-pick the fix:

   ```bash
   git fetch origin
   git checkout -b fix/<short-description> origin/release-X.Y

   # Always cherry-pick the squash/merge commit from main (not individual PR commits)
   MERGE_SHA=$(gh pr view <N> --repo cloudpilot-ai/karpenter-provider-gcp --json mergeCommit --jq '.mergeCommit.oid')
   git cherry-pick -m 1 "$MERGE_SHA"

   git push https://github.com/cloudpilot-ai/karpenter-provider-gcp.git fix/<short-description>
   ```

2. Open a PR targeting `release-X.Y` (not `main`), get it reviewed and merged.

3. Tag the tip of `release-X.Y` and push:

   ```bash
   git fetch origin

   # Capture the exact commit SHA — never rely on HEAD when tagging
   COMMIT=$(git rev-parse origin/release-X.Y)
   echo "Tagging commit: $COMMIT"

   # Create a signed tag on the explicit SHA
   git tag -s vX.Y.Z "$COMMIT" -m "Release vX.Y.Z"

   # Verify the tag object points to the right commit before pushing
   git cat-file -p vX.Y.Z | head -3

   git push https://github.com/cloudpilot-ai/karpenter-provider-gcp.git vX.Y.Z
   ```

4. Create the GitHub release draft. Note: `target_commitish` must point to `release-X.Y`, not `main`:

   ```bash
   gh release create vX.Y.Z \
     --repo cloudpilot-ai/karpenter-provider-gcp \
     --title "vX.Y.Z" \
     --notes "For the full migration guide see [MIGRATION.md](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/vX.Y.Z/MIGRATION.md).

   $(gh api repos/cloudpilot-ai/karpenter-provider-gcp/releases/generate-notes \
       --method POST \
       --field tag_name="vX.Y.Z" \
       --field previous_tag_name="vX.Y.prev" \
       --field target_commitish="release-X.Y" \
       --jq '.body')" \
     --draft
   ```

5. Follow steps 5–7 of the minor release process above, substituting `X.Y.Z` for the patch version and running:

   ```bash
   git checkout release-X.Y

   make e2e-deploy \
     RELEASE_VERSION=X.Y.Z \
     E2E_PROJECT_ID=<PROJECT_ID> \
     E2E_SA_PATH=<SA_KEY> \
     E2E_LOCATION=<ZONE>

   make e2e-tests \
     E2E_PROJECT_ID=<PROJECT_ID> \
     E2E_SA_PATH=<SA_KEY> \
     E2E_LOCATION=<ZONE> \
     E2E_REGION=<REGION>
   ```
