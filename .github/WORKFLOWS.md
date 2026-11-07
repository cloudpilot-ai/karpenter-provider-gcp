# Workflow dependency maintenance

Action dependencies are locked in `.github/workflows/actions.lock`, including
transitive actions. The lockfile determines which commits run; readable tags or
branches in workflow files do not advance those commits automatically.

After adding a workflow or changing an action reference, use the pinned release
generator for lockfile maintenance (presubmit's temporary verifier is described below):

```sh
gh extension install github/gh-actions-lock --pin v0.4.1
gh actions-lock
gh actions-lock --verify
```

Commit both the workflow changes and generated lockfile. Never edit the lockfile
by hand. Review commit and repository-identity changes like dependency upgrades.
Do not automatically accept moved or unreachable references.

`gh actions-lock --verify-local` checks coverage offline, but does not verify
upstream provenance. Presubmit uses the networked `--verify` check. Existing
Dependabot configuration updates dependencies and lock entries for onboarded
workflows; new workflows still require generator onboarding.

Presubmit temporarily builds the verifier from pinned commit
`39dd6b04a53288324a194d616f80c70477020131` in `dm3ch/gh-actions-lock` instead
of installing released v0.4.1. That commit adds the anonymous fallback for
public repositories whose organization IP allowlist rejects authenticated API
access ([#275](https://github.com/github/gh-actions-lock/pull/275) for
[#274](https://github.com/github/gh-actions-lock/issues/274)); released v0.4.1
fails closed on `aquasecurity/setup-trivy`. Keep the pinned commit literal and
its `HEAD` assertion, retire both when a release includes the fix, and update
the generator version only after it does. The verifier source is built under
`$RUNNER_TEMP` so its `go.mod` is not discovered by provider Make targets. In
that workflow, `GH_TOKEN` is scoped to the verification step, not the build.

The lockfile covers action code, not tools downloaded by actions, container
images, or job-level reusable-workflow calls. Repository policy enforcement is a
separate settings decision; adding this lockfile does not change those settings.
For onboarded workflows, native lockfiles also satisfy GitHub's **Require actions
to be pinned to a full-length commit SHA** policy without SHA literals in YAML.
Enabling that repository policy remains an administrator task.

See [GitHub's lockfile documentation](https://docs.github.com/en/actions/concepts/security/workflow-dependency-lockfiles).

## Security scanning

Security checks run on every push to `main`, daily, and on manual dispatch.
The daily scan detects newly published advisories even when source has not changed.
These checks detect findings after merge; they do not automatically fix dependencies
or gate the independent image-publishing workflow.

- Govulncheck blocks on reachable Go vulnerabilities for Linux amd64.
- Trivy scans controller images for Linux amd64 and arm64. Only fixable HIGH or
  CRITICAL **OS-package** findings block its job. Go findings remain in the complete
  JSON and SARIF reports with their original severity; govulncheck supplies the Go
  enforcement policy. Its amd64 analysis does not cover arm64-specific Go code.
- `UNKNOWN` means advisory severity metadata is unavailable, not that a finding is
  harmless. Do not assign a synthetic score or suppress it to clean up the dashboard.

Reports are uploaded even after a vulnerability gate fails, including valid reports
with zero findings. GitHub can automatically mark absent findings fixed after
processing the next report on `main` for the same tool and analysis configuration.
Keep SARIF categories and finding identities stable, and refresh both Trivy
architecture configurations. Govulncheck does not close Trivy alerts or vice versa.
A skipped scan or failed upload is not a clean result; retired configurations can
leave stale alerts. Reducing scan scope can also make findings disappear without
remediation, so do not mistake that for a dependency fix.

See [GitHub's alert-resolution documentation](https://docs.github.com/en/code-security/how-tos/manage-security-alerts/manage-code-scanning-alerts/resolve-alerts)
and [Go's severity-label rationale](https://go.dev/doc/security/vuln/).
