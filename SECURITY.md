# Reporting Security Issues

Please report potential vulnerabilities privately to [CloudPilot AI Security](mailto:security@cloudpilot.ai), not in public GitHub issues. We investigate security reports and address confirmed vulnerabilities as quickly as possible.

## Security checks

- **gosec and CodeQL** inspect source code and workflow security patterns.
- **Dependabot** detects known vulnerable module dependencies and proposes supported dependency/Actions updates. **Dependency Review** rejects PRs introducing high/critical dependency advisories.
- **govulncheck** (`make vulncheck`, pinned in `hack/tools`) checks controller source and unit-test packages against the Go vulnerability database, including the selected compiler's standard library. Reachable findings fail the check. SARIF is uploaded to GitHub Code Scanning on trusted events and retained as an artifact on fork PRs. Developer-tool modules and all build configurations are not covered by this source scan.
- **Trivy** checks OS and Go packages in both architectures of published controller images. All severities and unfixed findings are retained as JSON/SARIF; fixable high/critical findings fail the scan job. Scanner/database errors also fail. The daily/main security workflow resolves the latest GitHub release tag to an immutable image digest and uploads that release baseline to Code Scanning on `main`.

Release-tag scans run in a separate job **after** image publishing, signing, and provenance complete. Their JSON/SARIF reports are workflow artifacts, not branch Code Scanning uploads. These scans detect issues after publication; they are not a pre-publication gate and do not roll back releases. Scheduled scans cover the latest release, not every historical release or deployed image.

SBOMs describe components; signatures and provenance establish artifact identity/origin. None is a vulnerability verdict. Advisory databases can lag certificate distrust and other incidents: review upstream Go/base-image notices as well as scanner results. Inspect the actual artifact and release a rebuilt image when fixes are needed; changing source pins does not patch existing images.

Maintainers should enable dependency graph, Dependabot alerts/security updates, and require the Dependency Review and govulncheck checks through repository rules. Workflow files do not change those settings. Coupled Kubernetes dependencies and other intentional Dependabot exclusions still require manual security triage.

## Compiler and base maintenance

`Update security pins` runs weekly and can be dispatched manually (dry-run by default). It proposes one draft PR updating only the four module Go directives within their existing minor release and the allowlisted Chainguard static base index in `.ko.yaml`. It verifies linux/amd64 and linux/arm64 coverage and never auto-merges. Minor Go upgrades, separate toolchain directives, and registry/base changes need manual review. Digest pinning controls build inputs; upstream digest retention is not guaranteed indefinitely.

The workflow uses **`${{ github.token }}`** with job-scoped `contents: write` and `pull-requests: write`, signed commits, and DCO signoff; no App or PAT is needed. Repository/organization policy must allow Actions to create pull requests. The workflow deliberately runs only in the canonical repository and commits only the pin files.

PRs created/updated using this token normally **do not trigger other Actions workflows**. A maintainer must close and reopen the generated draft PR to trigger a human-originated PR event, then verify presubmit and security checks before merging. Repeat this after bot updates. Do not interpret absent checks as a successful validation. A manual dry run only resolves and reports proposed changes; it does not open a PR.

Local updater checks:

```sh
make security-test
# Requires crane; performs read-only upstream queries, without editing files:
bash hack/update-security-pins.sh --dry-run
```
