# Mirror and verify an existing container image

This composite action copies one fixed OCI root digest, including all platforms,
recursive referrers, and legacy digest tags such as Cosign `.sig` and `.att` tags.
It never builds an image, writes a source registry tag, or deploys a workload.

Onboard is private. Each publisher vendors the same committed action so public
repositories and private repositories do not depend on cross-repository token
permissions or organization action-sharing settings. Check out the publisher's
own repository before invoking its local action:

```yaml
- name: Mirror the image produced by the build
  uses: ./.github/actions/mirror-container-image
  with:
    source-ref: ${{ steps.build.outputs.source-ref }}
    target-repository: ${{ vars.ALIYUN_REGISTRY }}/cloudpilotai/example
    immutable-tags-json: '["v1.2.3"]'
    mutable-tags-json: '[]'
  env:
    ACR_USERNAME: ${{ secrets.ALIYUN_REGISTRY_USERNAME }}
    ACR_PASSWORD: ${{ secrets.ALIYUN_REGISTRY_PASSWORD }}
```

The canonical implementation and maintenance scripts live in Onboard. From an
Onboard checkout, after committing a reviewed change there, synchronize all
publisher copies from that full commit SHA:

```bash
python3 hack/mirror/vendor_action.py --source-commit <full-onboard-commit-sha> /path/to/publisher /path/to/another-publisher
python3 hack/mirror/vendor_action.py --source-commit <full-onboard-commit-sha> --check /path/to/publisher /path/to/another-publisher
```

The script copies exactly eight canonical action files, excluding Python caches,
and adds `PROVENANCE.json` containing the full Onboard source commit and SHA-256
of every copied file. It verifies that canonical working files match that commit
before copying; `--check` detects content or provenance drift without writing.
Unexpected files outside `__pycache__` are rejected and never deleted.
Publisher updates must commit the action copy, its provenance, and any workflow
changes together. Never edit a vendored copy independently. Onboard's canonical
action has no provenance file referencing itself.

`verify-images.sh` is the standalone installer preflight. Run `bash
verify-images.sh images.txt`, where each line is a complete
`registry/repository@sha256:...` reference. For installers retaining runtime tags,
add `--tags-file bindings.tsv`; each binding is `registry/repository:tag`, a tab,
and the expected `sha256:...`. It checks current tag contents, recursively hashes
manifest downloads, and checks blob/config/layer availability and sizes with
HEAD requests. Full layer downloads remain part of publication verification.
The helper uses anonymous Bearer challenges, ignores curl configuration and
credential environments, and performs only GET/HEAD requests. It requires Bash,
curl, jq, and standard shell utilities; production endpoints must use HTTPS.

`source-ref` must contain an explicit registry, repository, and complete
`@sha256:...` digest. A source tag, including a tag combined with a digest, is
rejected. Use the digest returned by the completed ko or Buildx publication;
never resolve an unrelated upstream tag to reconstruct a historical release.

The target repository is generic so that the same primitive can preserve an
existing DockerHub publication. Registry credentials are supplied only through
`ACR_USERNAME` and `ACR_PASSWORD`, regardless of the registry vendor. Public
anonymous download of the destination and artifact inventory is mandatory.

## Request and receipt contract

Before downloading tools or writing to a registry, the action creates a read-only
`request.json` and `request.sha256` in a unique directory below
`${RUNNER_TEMP}/mirror-container-image/`. The checksum is checked again before
execution. A publisher should also save the same request immediately after its
build so that failures before invoking this action do not lose the source digest.

```json
{
  "schemaVersion": 1,
  "sourceRef": "public.ecr.aws/cloudpilotai/example@sha256:<64 lowercase hex characters>",
  "targetRepository": "approved-acr-host/cloudpilotai/example",
  "immutableTags": ["v1.2.3"],
  "mutableTags": []
}
```

Outputs are `request-path`, `receipt-path`, `mirror-ref`, and `digest`. The last
two identify the requested destination; they do not independently prove success.
Consumers must parse `receipt.json` and require **both** `verified: true` and
`status: complete`. Failed attempts write a receipt with `verified: false` and
`status: failed`. No failed receipt authorizes an installer or chart update.

A successful receipt contains these stable fields:

| Field | Meaning |
| --- | --- |
| `schemaVersion` | `1` |
| `source` | Original complete `repository@sha256:...` reference |
| `target` | Destination complete reference with the same root digest |
| `digest` | Root index or image digest, including `sha256:` |
| `requestSHA256` | SHA-256 of the archived canonical request file |
| `platforms` | Sorted runtime platforms read from image configs |
| `tags` | Requested aliases, each anonymously read back at the root digest |
| `contentTag` | Internal retention tag, or null for verification-only runs |
| `artifacts` | Recursive referrer and legacy digest-tag inventory |
| `verification` | Manifest equality, complete blob download, artifact coverage, and tag checks |
| `publisher` | GitHub repository/run/attempt/workflow revision when available |
| `tool`, `verifiedAt` | Fixed client version and UTC verification time |

Reports contain no credential files. Temporary registry credentials and the
downloaded binary live outside the report tree, including if execution is killed
before cleanup. Upload requests and receipts with `if: always()`; for example:

```yaml
path: |
  mirror-requests/
  ${{ runner.temp }}/mirror-container-image/**/request.json
  ${{ runner.temp }}/mirror-container-image/**/request.sha256
  ${{ runner.temp }}/mirror-container-image/**/receipt.json
```

Actions artifacts expire. Release owners must archive completed locks and source
requests with their long-lived release/delivery metadata before those artifacts
expire. Merely retaining a mutable source tag does not preserve release identity.

## Copy and verification behavior

The fixed client is regctl v0.11.6, downloaded with GitHub CLI and checked against
embedded per-platform SHA-256 values. The action copies raw manifests and blobs
using `--force-recursive --referrers --digest-tags`, without platform selection,
manifest conversion, or layer recompression. It fails if artifact inventory
cannot be read; it does not interpret an authentication failure as an empty list.
Foreign layers containing external URLs are rejected until a delivery policy
explicitly supports them.

The staging/retention tag is `mirror-sha256-<hex>`. The unprefixed
`sha256-<hex>` namespace is reserved by OCI referrer fallback indexes and must
never be used as the image's retention tag.

The verifier uses an isolated regctl configuration with Docker credential and
certificate import disabled. It downloads the complete destination manifest and
blob graph anonymously, checking every SHA-256 and size. It also checks recursive
referrers and legacy tags, including artifacts attached to platform manifests.
Buildx `unknown/unknown` attestations are retained but do not count as runnable
platforms. Copied signatures retain their original identity; this action does
not claim that a destination-specific signature trust policy has been verified.

Source artifact inventory is checked again after copying. If target verification
fails, a complete source re-read must prove that its inventory changed before
another copy is attempted. Unchanged-source verification failures, failed source
re-reads, and target discovery failures stop immediately. Changing inventories
allow at most three copy attempts; persistent changes fail. Every successful
attempt still requires full target verification and a stable source readback.
Transport operations separately have bounded retries for transient failures.
Authentication failures, digest mismatches, and stable-tag conflicts never count
as successful verification.

Existing immutable tags at a different digest fail before copying and are checked
again before promotion. Same-digest retries succeed without rewriting version
tags. Mutable aliases change only when listed explicitly. An alias failure can
leave copied content or some already-correct aliases behind; retry the exact
request to finish. Nothing is deleted or rolled back automatically.

Registry tag writes are not an atomic multi-tag transaction. A single publisher
must own each destination repository, and ACR immutable-tag policy must protect
release tags against concurrent writers. GitHub concurrency groups do not span
repositories. Historical backfill should leave `mutableTags` empty and must be
scheduled separately from a live owner publication.

## Retry and read-only verification

The two manual workflows below exist only in `cloudpilot-ai/onboard`; they are
not included in vendored publisher repositories. Maintainers with Onboard access
can use them after the relevant workflow change is merged. Publishers can also
retry the identical archived request with the standalone CLI below, directly
from their own checkout, without accessing Onboard.

For an Onboard mirror-only retry, start a **new** run of
`Mirror existing container images` from Onboard's trusted default branch. Supply
the archived source digest, image path below `cloudpilotai/`, and the original tags.
Its destination is `vars.ALIYUN_REGISTRY/cloudpilotai/<image>`. It performs no
builds, source-tag updates, chart publication, or deployment. A previous failed
release run does not load newly merged workflow code when rerun.

Onboard's separate `Publish a future third-party image version` workflow publishes new
VictoriaMetrics, kube-state-metrics, or workload-identity-probe versions. It
requires an approved upstream digest and immutable version tag, copies the same
bytes into the default ECR repository first, and then mirrors that ECR digest to
ACR for VictoriaMetrics and kube-state-metrics. The probe mirrors its original
upstream digest to ACR after ECR verification so its receipt preserves the
`curlimages/curl` source used by existing frozen Onboard manifests. All copies
retain the same root digest. The workflow validates both destinations and credential presence before registry
writes and never builds an image. Historical backfill must use the actual
published ECR digest through the mirror-only workflow instead; a same-named
upstream version may contain different bytes.

Both third-party requests are saved before publication. ECR evidence is retained
in `third-party-ecr-<run>-<attempt>`, while ACR evidence uses
`image-mirror-<run>-<attempt>`, matching mirror-only delivery consumers. Only the
ACR artifact should be passed to an ACR installer generator. The ECR login
password stays in one shell/child-process environment and is never written to a
step output or environment file.

The standalone CLI is included in every vendored copy. From the publisher's own
checkout, install the client and retry the identical archived `request.json`
without rebuilding or resolving a mutable source tag. Supply the original
request checksum when available and retain the new report for delivery consumers:

```bash
bash .github/actions/mirror-container-image/install-regctl.sh /tmp/mirror-tools
python3 .github/actions/mirror-container-image/mirror.py run \
  --request-file request.json --regctl /tmp/mirror-tools/regctl \
  --report-dir ./mirror-retry-report
```

If the archive includes `request.sha256`, also pass
`--request-sha256 "$(cat request.sha256)"`. Publishing requires the approved
destination credentials in `ACR_USERNAME` and `ACR_PASSWORD`; obtain them through
the publisher's existing secret mechanism. Requests contain no credentials.

`verify` is strictly read-only against registries, requires no ACR push credential,
and produces the same receipt contract for an already mirrored dependency:

```bash
python3 .github/actions/mirror-container-image/mirror.py verify \
  --request-file request.json --regctl /tmp/mirror-tools/regctl \
  --report-dir /tmp/new-dependency-receipt
```

`--report-dir` must be new or contain the exact same immutable request. The hidden
`--test-http-localhost` switch is only for loopback integration fixtures and is
not exposed through the composite action.

## Onboard validation

Run the following from an Onboard checkout. The `hack/mirror/` test programs and
maintenance scripts are not part of vendored publisher copies:

```bash
python3 hack/mirror/test_mirror.py
python3 hack/mirror/test_vendor_action.py
python3 hack/mirror/integration.py --regctl /tmp/mirror-tools/regctl
```

The integration test starts two auto-removed Distribution 3.0.0 containers and a
loopback proxy requiring credentials for writes while allowing anonymous reads.
Synthetic fixtures cover two runtime architectures, root and child referrers,
legacy signatures, anonymous full downloads, immutable conflict rejection,
idempotent retry, a read-only verifier, and corrupt-blob rejection. It never
contacts or changes a customer or production registry.
