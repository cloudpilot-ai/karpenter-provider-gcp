# Security regression gating

`regression_gate.py` compares a baseline scanner report with a candidate report and fails
only for blocking findings the candidate newly introduces. `.github/workflows/security.yaml`
owns every dedicated security check; `gosec` stays in ordinary golangci-lint and workflow
hardening stays in each workflow.

## Policy

- A pull request blocks when it introduces a new blocking finding. Findings that already
  exist in the pull request base stay visible and are not PR failures.
- zizmor: severity `High` blocks (zizmor has no `Critical`).
- govulncheck: any newly reachable vulnerable symbol blocks. govulncheck carries no
  severity, and a newly reachable finding blocks even when severity is unknown.
  Module- and package-level findings record import exposure only and are report-only.
- Grype: fixable `High`/`Critical` **OS package** findings block (`deb`, `rpm`, `apk`,
  `alpm`, `portage`). Library findings and findings without a fix stay report-only.
  Complete image findings, including libraries, are retained as reports.
- dependency-review keeps its native `high` threshold and CodeQL keeps native new-alert
  evaluation; see [Repository configuration](#repository-configuration).
- Tool errors, missing or invalid reports, and reports produced by different scanner or
  database versions fail the check explicitly. They are never treated as a pass.

## Jobs

| Job | Events | Gate |
| --- | --- | --- |
| `dependency-review` | PR | dependency-review severity threshold |
| `actions-lock` | all | `gh actions-lock --verify` (pinned v0.4.1) |
| `codeql` (`Analyze (go)`, `Analyze (actions)`) | PR, main, schedule, manual | code scanning ruleset, not the workflow |
| `zizmor-regression` | PR | `regression_gate.py --scanner zizmor` |
| `go-vulnerability-regression` | PR | `regression_gate.py --scanner govulncheck` |
| `image-security-regression` | PR | `regression_gate.py --scanner grype`, per platform |
| `govulncheck` | main, schedule, manual on main | text-mode reachable-vulnerability gate + SARIF |
| `container-image` | main, schedule, manual on main | fixable `High`/`Critical` OS-package gate + SARIF |
| `zizmor-report` | main, schedule, manual on main | SARIF publication only |

The consolidated schedule is daily for every job, including CodeQL, which previously ran on
its own weekly cron. Main and scheduled runs keep full scans and SARIF publication; only pull
requests compare paired reports.

## Comparable paired scans

- The baseline is the exact pull request base SHA (`github.event.pull_request.base.sha`); the
  candidate is the proposed merge commit. Stale main artifacts are never used.
- Scanner versions are pinned in the workflow `env:` block and invoked directly instead of
  through a checkout's `make` targets: zizmor 1.32.0 (PyPI wheel), govulncheck v1.9.0
  (`go install`), Grype v0.118.0 (download action), ko v0.19.1.
- zizmor runs with `--format json --no-progress --offline --strict-collection --no-exit-codes` from each checkout
  root, so it is a real git checkout that recognizes `.github/workflows/actions.lock` and
  reports findings instead of an exit code. A non-zero exit is a tool error.
- The Go vulnerability database is resolved once per run from `golang.org/x/vulndb@latest`,
  then the resulting immutable module directory is passed to both scans as
  `-db file://<module>/data/osv`. This keeps advisories current without changing the database
  between revisions. Two successive requests to `vuln.go.dev` are not a snapshot.
  `govulndb.txt` records the module version and module hash. Each revision is scanned with its
  own intended toolchain (`GOTOOLCHAIN` from that revision's `go.mod`, recorded in
  `go-version-<revision>.txt`) so standard-library results stay observable across Go upgrades;
  `GOOS`/`GOARCH` are identical for both scans. govulncheck's JSON output exits zero on
  findings, so the comparator is the gate.
- Images: `grype db update` runs once into `$RUNNER_TEMP/grype-db`, then
  `GRYPE_DB_AUTO_UPDATE=false` for all four scans (two revisions × amd64/arm64). Each report
  embeds the database identity (`descriptor.db.status`), and the comparator fails when the two
  reports were produced against different databases. Images are built locally with
  `ko build --push=false`; four builds per pull request are expected, and the timeout absorbs
  them rather than skipping any scan. Locally measured: 10–25 s per ko build with a warm Go
  cache and 1–2 s per Grype scan; the CI timeout is 75 minutes for cold builds plus the
  database download.

## Local commands

```bash
make security-gate-test                 # comparator unit tests
python3 hack/security/regression_gate.py --scanner zizmor \
  --baseline reports/zizmor-base.json --candidate reports/zizmor-candidate.json
python3 hack/security/regression_gate.py --scanner govulncheck \
  --baseline reports/govulncheck-base.json --candidate reports/govulncheck-candidate.json
python3 hack/security/regression_gate.py --scanner grype --context linux/amd64 \
  --baseline reports/grype-base-amd64.json --candidate reports/grype-candidate-amd64.json
make vulncheck                          # local reachable-vulnerability scan (upstream database)
```

`--summary-out` and `--json-out` write the human-readable and machine-readable summaries.
Exit codes: `0` no new blocking findings, `1` new blocking findings, `2` report or comparison
error. Use `zizmor --offline --min-severity high .` for the PR gating severity locally.

## Artifacts

Pull requests retain full native reports, both summary formats, the database identities, the
per-revision Go version, and the ko build logs in the `zizmor-regression`,
`go-vulnerability-regression`, and `image-security-regression` artifacts. Artifacts upload even
when the policy gate fails. Main runs keep `govulncheck-sarif`,
`main-image-security-linux-<arch>`, and `zizmor-sarif`.

## Known limitations

- zizmor identity is the workflow path plus the primary symbolic YAML route, so inserting or
  reordering steps, renaming jobs, or moving a workflow file re-reports inherited findings as
  new. This conservative re-gating after structural moves is accepted; line and whitespace
  edits are stable. Identity deliberately excludes the flagged content, so replacing an
  already-flagged step's action with a different unpinned action at the same route is not new.
- Multiple zizmor findings inside one YAML leaf fold into one identity, while multiplicity is
  preserved: adding a further occurrence of an inherited finding still blocks.
- govulncheck folds caller paths: a new call path into an already reachable vulnerable symbol
  is not new exposure.
- Grype identity excludes package versions, so an unchanged vulnerable exposure survives a
  version change. An existing report-only exposure becomes blocking when it crosses the
  High/Critical threshold or becomes fixable.
- Code scanning merge protection evaluates alerts whose source lines appear in the pull request
  diff. That differs from these whole-tree paired comparisons and is not a substitute for them.
- The pull request's own workflow and comparator are executed. Read-only permissions stop
  privileged side effects but cannot make the gate tamper-proof; reviewing workflow and script
  changes remains the control. `pull_request_target` is deliberately not used.
- Fork pull requests run on the base repository's runners and spend minutes on four image
  builds and the database downloads.

## Repository configuration

Not applied by this change; a maintainer must configure it.

Required status checks. `dependency-review`, `presubmit`, `Analyze (go)`, and
`Analyze (actions)` keep their existing names across the move. The new check names are
`Workflow security regression`, `Go vulnerability regression`, and
`Image security regression` (`Verify action dependencies` was previously a step inside
`presubmit`, so it is a new check name).

Code scanning merge protection (Settings → Rules → New ruleset → Code scanning):

- Select CodeQL, require security alerts at `High` or higher, and choose the non-security
  alert threshold deliberately — CodeQL `error` results for non-security queries are not a
  security signal.
- The ruleset applies to pull requests. Merge queue gating is governed by required status
  checks, so code scanning merge protection does not carry into merge groups.

Bootstrap after merge. Moving the CodeQL jobs to another workflow file does not by itself
guarantee analysis continuity for a category even when the categories `/language:go` and
`/language:actions` are unchanged. After merging, run the workflow on `main` once and confirm
the SARIF upload, then open a pull request and confirm the `Analyze (…)` checks and the three
regression jobs report.

## Changing pinned versions

Update `ZIZMOR_VERSION`, `GOVULNCHECK_VERSION`, `GRYPE_VERSION`, and `KO_VERSION` in
`.github/workflows/security.yaml`. Keep `GOVULNDB_MODULE` resolving `@latest` once per run;
do not hard-pin the advisory database to a historical date. Bump zizmor in both the regression job and the SARIF job. After changing
workflow action dependencies, regenerate the lockfile with the pinned tool and commit the
result:

```bash
gh extension install github/gh-actions-lock --pin v0.4.1
gh actions-lock
gh actions-lock --verify
```
