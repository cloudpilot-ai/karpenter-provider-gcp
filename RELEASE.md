# Release Process

## Cadence

- **Minor versions** (`vX.Y.0`): released monthly, targeting the first week, when there are changes to ship.
- **Patch versions** (`vX.Y.Z`): released as needed for critical bug fixes, cherry-picked onto the active release branch.

## Code Freeze

One week before a minor release:

- No new features are reviewed or merged.
- Only release-critical bug fixes and stabilization PRs are accepted.

---

## Release notes (all releases)

Use [.github/RELEASE_NOTES_TEMPLATE.md](.github/RELEASE_NOTES_TEMPLATE.md) for **Highlights**, the only routinely authored section. The maintainer supplies/selects the wording; an LLM may propose evidence-grounded wording for approval. Git-history tooling must not choose highlights or infer user benefits.

The release command automatically prepends the tagged migration-guide link. GitHub's native release-note generation supplies **What's Changed**, its categories, **New Contributors** when applicable, and the **Full Changelog** comparison link. Do not manually fill separate upgrade, changelog, or contributor template sections.

Review the generated draft before publication:

- Reconcile it against every commit in the immutable previous-tag-to-release-tag range, resolve associated merged PRs with complete API pagination, and account for direct commits. Do not rely on date searches or a truncated display.
- Review category and exclusion rules in `.github/release.yml`. The final catch-all includes uncategorized PRs; dependency labels alone do not exclude substantial upgrades. Review deliberate exclusions, including bot-authored changes, rather than assuming all generated notes are complete.
- Native GitHub attribution does not reliably credit original authors when a maintainer finishes/replaces a PR. Check linked original PRs and commit authorship, and flag or correct exceptional credit gaps before publication; reviewers/discussion participants are not automatically authors. This is draft review, not a routinely hand-authored contributor section.
- Verify links and claims, obtain maintainer approval of Highlights, and remove all Highlights placeholders/comments before creating the draft. Before publication, resolve any completeness or attribution findings. Generated notes are automatic, not exempt from review.

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

Open a *"chore: prepare vX.Y.0 release"* PR and add the `release-prep` label (excludes it from the auto-generated changelog). Then run the full e2e suite — the PR only touches docs, so this validates the release candidate code before the merge commit is tagged:

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

### 4. Wait for builds and verify published artefacts

The tag push starts the [release build workflow](.github/workflows/release-build.yaml), which publishes the controller image and Helm charts independently of the GitHub release. Before creating or publishing the GitHub release:

- Confirm the workflow run is for the exact signed tag and release commit.
- Wait for both `image-publish` and `package-chart` to complete successfully. Stop on failure; do not overwrite the tag or automatically rerun publication.
- Verify that the published image and both charts are available, then verify their signatures and build provenance using the commands below. Workflow success alone is not sufficient evidence of artifact availability or successful client-side verification.

```bash
RELEASE_VERSION=X.Y.0

# Docker image (ECR Public)
TOKEN=$(curl -s "https://public.ecr.aws/token/?scope=repository:cloudpilotai/gcp/karpenter:pull" | jq -r '.token')
curl -fsS \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  "https://public.ecr.aws/v2/cloudpilotai/gcp/karpenter/manifests/v${RELEASE_VERSION}" \
  | jq -r '.mediaType'

# Helm charts (GitHub Pages)
curl -s "https://cloudpilot-ai.github.io/karpenter-provider-gcp/index.yaml" \
  | grep -A3 "version: ${RELEASE_VERSION}"
```

Also confirm the new version appears on [ArtifactHub](https://artifacthub.io/packages/helm/karpenter-provider-gcp/karpenter).

> **Check signatures:** Run the commands in [Verify release artifacts](docs/getting-started/installation.md#verify-release-artifacts-optional) with `VERSION=vX.Y.0` (note the leading `v`). This confirms that users can verify the published image signature, chart signatures, and build provenance. Fix any availability, signature, or provenance failure before proceeding to the GitHub release draft or publication. Confirm both the `karpenter` and `karpenter-crd` charts, not just one entry in the Helm index.

### 5. Create GitHub release

Prepare the Highlights file:

```bash
HIGHLIGHTS_FILE=$(mktemp /tmp/karpenter-release-highlights.XXXXXX)
cp .github/RELEASE_NOTES_TEMPLATE.md "$HIGHLIGHTS_FILE"
```

Fill only Highlights (or review an LLM proposal) following [Release notes](#release-notes-all-releases), remove the placeholder/comment, and get maintainer approval. Then create the draft; `--notes` prepends the migration link and Highlights to GitHub's generated notes, and `--notes-start-tag` fixes the comparison base:

```bash
gh release create vX.Y.0 \
  --repo cloudpilot-ai/karpenter-provider-gcp \
  --verify-tag \
  --title "vX.Y.0" \
  --generate-notes \
  --notes-start-tag "vA.B.C" \
  --notes "For the full migration guide see [MIGRATION.md](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/vX.Y.0/MIGRATION.md).

$(< "$HIGHLIGHTS_FILE")" \
  --draft
```

> `.github/release.yml` controls native changelog categories and exclusions. It does not generate Highlights or guarantee replacement-PR attribution; review those separately.

Review the draft at `https://github.com/cloudpilot-ai/karpenter-provider-gcp/releases`, then publish:

```bash
gh release edit vX.Y.0 --repo cloudpilot-ai/karpenter-provider-gcp --draft=false
```

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

4. Follow step 4 of the minor release process to wait for the tag-triggered builds and verify published artifacts, signatures, and provenance, substituting `X.Y.Z` for the version. Only after verification succeeds, prepare a maintainer-approved `HIGHLIGHTS_FILE` from the template as in minor step 5. Create the draft with native notes generated from the previous patch tag to the existing signed release tag:

   ```bash
   gh release create vX.Y.Z \
     --repo cloudpilot-ai/karpenter-provider-gcp \
     --verify-tag \
     --title "vX.Y.Z" \
     --generate-notes \
     --notes-start-tag "vX.Y.prev" \
     --notes "For the full migration guide see [MIGRATION.md](https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/vX.Y.Z/MIGRATION.md).

   $(< "$HIGHLIGHTS_FILE")" \
     --draft
   ```

5. Follow steps 5–7 of the minor release process above, substituting `X.Y.Z` for the patch version. For step 5, review and publish the draft created above rather than creating another release. Then validate the published release with:

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
