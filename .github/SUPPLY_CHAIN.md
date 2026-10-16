# Published artifact verification

The release workflow signs controller images and both Helm charts. The main-branch
workflow signs the `latest` image. Only the upstream repository publishes artifacts.
Signing applies to new builds; older releases are not retroactively signed.

## Controller images

Images use keyless Cosign signing with GitHub Actions OIDC and public Sigstore
certificates/transparency logging. The signature covers the immutable digest
returned by `ko`, including its multi-platform index, not a subsequently resolved
tag. The platform list remains controlled by `.ko.yaml`.

Use Cosign **v3.1.3**. Public ECR referrers support is not assumed: signatures use
Cosign's legacy `sha256-<digest>.sig` tags, not OCI 1.1 signature artifacts. Keep
these tags when configuring registry retention. Changing Cosign's storage format
requires coordinated signing, verification, and registry compatibility testing.

```sh
IMAGE=public.ecr.aws/cloudpilotai/gcp/karpenter@sha256:<digest>
TAG=v<version>
cosign verify --new-bundle-format=false --experimental-oci11=false \
  --certificate-identity "https://github.com/cloudpilot-ai/karpenter-provider-gcp/.github/workflows/release-build.yaml@refs/tags/$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com "$IMAGE"
```

For `latest`, use this exact certificate identity instead:

```text
https://github.com/cloudpilot-ai/karpenter-provider-gcp/.github/workflows/cloudpilot-latest-build.yaml@refs/heads/main
```

Do not accept an arbitrary GitHub certificate or disable transparency-log checks.

### SPDX SBOMs

`ko build --sbom=spdx` publishes SBOMs with the image. It also writes local SPDX
JSON files retained in the workflow's `release-image-sboms` or `latest-image-sboms`
artifact, alongside `image-reference.txt`. Workflow artifact downloads require
GitHub authentication and are subject to Actions retention; registry SBOMs are the
long-lived retrieval path.

With `crane` and Cosign installed, retrieve SBOMs for the index and each platform:

```sh
cosign download sbom "$IMAGE" > index.spdx.json
for PLATFORM in linux/amd64 linux/arm64; do
  DIGEST=$(crane digest --platform "$PLATFORM" "$IMAGE")
  cosign download sbom "public.ecr.aws/cloudpilotai/gcp/karpenter@$DIGEST" \
    > "${PLATFORM#linux/}.spdx.json"
done
```

These are `ko`'s SBOMs, not a separate full-filesystem vulnerability scan. SBOM
attachments are not separately signed by this workflow; image build attestations
are described below.

## Helm charts

The GitHub Pages repository publishes `.tgz.prov` files for `karpenter` and
`karpenter-crd`. These are native OpenPGP Helm provenance files, compatible with
`helm verify` and `helm install --verify` (including Helm 4). Standalone keyless
Cosign bundles are not a substitute for Helm's built-in verification.

Obtain the expected primary signing fingerprint from a trusted maintainer channel.
The repository publishes the corresponding public key at
`https://cloudpilot-ai.github.io/karpenter-provider-gcp/keys/<FINGERPRINT>.asc`.
Downloading a key from the chart repository alone does not establish trust.

```sh
FINGERPRINT=<trusted-uppercase-primary-fingerprint>
curl --fail --location \
  "https://cloudpilot-ai.github.io/karpenter-provider-gcp/keys/$FINGERPRINT.asc" \
  --output signing-key.asc
gpg --show-keys --with-fingerprint signing-key.asc
# Check the full primary fingerprint before using this key.
gpg --dearmor < signing-key.asc > trusted-helm-keys.gpg
helm pull karpenter-gcp/karpenter --version <version> --prov
helm verify karpenter-<version>.tgz --keyring trusted-helm-keys.gpg
# Repeat for karpenter-crd.
```

Pages publication retains historical provenance and fingerprint-named public keys.
`signed-charts.txt` records which archive versions require provenance; a missing
signature for those versions aborts deployment. A 404 is allowed for older unsigned
versions only. Keep this record and the `keys/` directory when migrating hosting.

## Build provenance

Both image publishing workflows and the chart packaging job create signed
[SLSA build provenance](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
using `actions/attest-build-provenance`. Image subjects use the exact published
index digest; chart subjects use the two newly packaged archive hashes.
Attestations are stored by GitHub, **not** pushed to Public ECR. Helm PGP provenance
and build provenance are different: verify both if your policy requires both.

Use GitHub CLI with artifact attestation support and GitHub authentication:

```sh
gh attestation verify "oci://$IMAGE" \
  --repo cloudpilot-ai/karpenter-provider-gcp \
  --signer-workflow cloudpilot-ai/karpenter-provider-gcp/.github/workflows/release-build.yaml \
  --source-ref "refs/tags/$TAG"
gh attestation verify karpenter-<version>.tgz \
  --repo cloudpilot-ai/karpenter-provider-gcp \
  --signer-workflow cloudpilot-ai/karpenter-provider-gcp/.github/workflows/release-build.yaml \
  --source-ref "refs/tags/$TAG"
```

Repeat for `karpenter-crd`. For `latest`, select `cloudpilot-latest-build.yaml` and
`refs/heads/main`. These attestations identify the workflow, source commit, and
artifact; they do not claim a hermetic or reproducible build, or a particular SLSA
level. They do not attest an independently generated SBOM or every base-image
component.

## Maintainer setup

Before enabling a chart release, provision these separately in the upstream
repository (or the existing `github-pages` environment):

| Setting | Purpose |
| --- | --- |
| Secret `HELM_SIGNING_KEY_BASE64` | Base64-encoded, exported OpenPGP private signing key |
| Secret `HELM_SIGNING_KEY_PASSPHRASE` | Nonempty passphrase protecting that key |
| Variable `HELM_SIGNING_KEY_FINGERPRINT` | Uppercase, full 40-character primary fingerprint |

Use a dedicated, passphrase-protected OpenPGP v4 signing key compatible with Helm
(e.g. RSA). The helper validates the fingerprint, exports only that primary key,
and gives Helm its actual UID selector. An isolated temporary keyring is removed
on exit; no private key or passphrase is placed in the Pages directory.

Publish the fingerprint through a trusted channel. Once the identity is chosen,
maintainers can add the following real metadata to both charts for Artifact Hub's
signing-key display; this PR intentionally does not invent a fingerprint:

```yaml
annotations:
  artifacthub.io/signKey: |
    fingerprint: <FULL_PRIMARY_FINGERPRINT>
    url: https://cloudpilot-ai.github.io/karpenter-provider-gcp/keys/<FULL_PRIMARY_FINGERPRINT>.asc
```

Retain old fingerprint-named keys during rotation. Do not overwrite an old key's
file with a different key. No signing secrets are needed for the image keyless
flow; existing AWS publishing credentials remain unchanged.

### Release prerequisites and limitations

- Missing chart signing configuration fails closed. Provision secrets before
  merging/enabling release tags; this change does not provision them.
- The publishing jobs need `id-token: write`; GitHub build attestations also need
  `attestations: write`. The repository must support GitHub artifact attestations.
- Local tests cover encrypted-key chart signing and restoration, strict image
  references, and verification command construction. They do **not** validate
  GitHub OIDC issuance, public Sigstore service availability, or Public ECR write
  permissions. Validate these on an explicitly authorized upstream release.
- Cosign's pinned legacy mode avoids depending on OCI 1.1 referrers, but still
  requires ECR permission to publish digest-derived signature tags and continued
  public Sigstore support for the legacy signature format.
- Image and chart jobs are not atomic: an image can publish even if chart signing
  fails. Pages jobs are serialized to avoid concurrent snapshot replacement.

Run the offline helper tests with `make signing-test` (Bash, Python 3, GnuPG, and
Helm required). CI tests against the same Helm 4 version used for publication.
