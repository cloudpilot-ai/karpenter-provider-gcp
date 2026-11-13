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

The tag push starts the [release workflow](.github/workflows/release.yaml), which publishes the controller image and Helm charts independently of the GitHub release. When resuming a previously tagged release, use the workflow path in that tag: older releases, including `v0.8.0`, used `release-build.yaml`, which remains part of their signing identity. Before creating or publishing the GitHub release:

- Confirm the workflow run is for the exact signed tag and release commit.
- Wait for both `image-publish` and `package-chart` to complete successfully. Stop on failure; do not overwrite the tag or automatically rerun publication.
- Verify that the published image and both charts are available, then verify their signatures and build provenance using the commands below. Workflow success alone is not sufficient evidence of artifact availability or successful client-side verification.

```bash
RELEASE_VERSION=X.Y.0

# Test anonymous access with empty registry credentials (no deployment).
VERIFY_DIR=$(mktemp -d)
(
  export DOCKER_CONFIG="$VERIFY_DIR/docker"
  export HELM_REGISTRY_CONFIG="$VERIFY_DIR/helm-registry.json"
  mkdir -p "$DOCKER_CONFIG"
  # Explicit empty entries prevent Podman fallback and native-helper auto-detection.
  printf '%s\n' '{"auths":{"ghcr.io":{}}}' > "$DOCKER_CONFIG/config.json"
  cp "$DOCKER_CONFIG/config.json" "$HELM_REGISTRY_CONFIG"
  crane manifest "ghcr.io/cloudpilot-ai/karpenter-provider-gcp:v${RELEASE_VERSION}" | jq -r '.mediaType'
  helm pull oci://ghcr.io/cloudpilot-ai/karpenter-provider-gcp/charts/karpenter \
    --version "$RELEASE_VERSION" --destination "$VERIFY_DIR"
  helm pull oci://ghcr.io/cloudpilot-ai/karpenter-provider-gcp/charts/karpenter-crd \
    --version "$RELEASE_VERSION" --destination "$VERIFY_DIR"
)
rm -rf "$VERIFY_DIR"
```

Also confirm the new version appears on [ArtifactHub](https://artifacthub.io/packages/helm/karpenter-provider-gcp/karpenter).

> **Check signatures:** Run the commands in [Verify release artifacts](docs/getting-started/installation.md#verify-release-artifacts-optional) with `VERSION=vX.Y.0` (note the leading `v`). This confirms that users can verify the published image signature, chart signatures, and build provenance. Fix any availability, signature, or provenance failure before proceeding to the GitHub release draft or publication. Confirm both the `karpenter` and `karpenter-crd` OCI charts, including anonymous access with supported Helm 3/4 clients.

### 5. Create GitHub release

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

`RELEASE_VERSION` switches `make e2e-deploy` into published-chart mode — it installs from GHCR OCI at the given version with no local image build. For a historical release still on Pages, explicitly set `RELEASE_CHART_SOURCE=pages`; there is no fallback after an OCI failure. `RELEASE_OCI_REPOSITORY` overrides the OCI chart prefix when validating an approved mirror. All specs must pass before announcing.

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

4. Follow step 4 of the minor release process to wait for the tag-triggered builds and verify published artifacts, signatures, and provenance, substituting `X.Y.Z` for the version. Only after verification succeeds, create the GitHub release draft. Note: `target_commitish` must point to `release-X.Y`, not `main`:

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
