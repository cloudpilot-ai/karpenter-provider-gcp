# Comparator test fixtures

Each fixture is native scanner output, not a hand-built document. Tests that need a scenario
the pinned scanner cannot produce on demand derive their report from these files in memory and
rewrite only the field under test, so the surrounding document keeps its native shape.

## zizmor 1.32.0

`zizmor --format json --no-progress --offline --no-exit-codes .` in a throwaway directory
containing one `.github/workflows/ci.yaml`, plus variants of that workflow:

- `candidate-line-shift.json`: same workflow with a comment and blank lines added.
- `candidate-added-step.json`: an extra step using an unpinned action.
- `candidate-different-site.json`: the unpinned step moved to a new job.
- `candidate-same-route-replacement.json`: the flagged action replaced at the same step.
- `candidate-report-only.json`: adds `.github/dependabot.yml`, producing `Medium` and `Low`
  findings only.

## govulncheck v1.9.0

`govulncheck -db file:///tmp/vulndb-snapshot -format json` against a module that requires
`filippo.io/edwards25519` and calls `(*Point).MultiScalarMult` (GO-2026-4503):

- `baseline-clean.json`: the fixed module version, no findings.
- `baseline-reached.json`: the vulnerable version, with module-, package-, and symbol-level
  findings.
- `candidate-reached-wrapped.json`: the same vulnerable call behind extra helper functions, so
  caller frames and line numbers differ while the reached symbol does not.
- `candidate-package-level.json` / `baseline-clean-package-level.json`: `-scan package`.
- Only the `config`, `progress`, `finding`, and the first `osv` messages are kept; the pinned
  tool emits one `osv` message per entry in the whole database, which the comparator ignores.

## Grype v0.118.0

`grype docker-archive:alpine.tar --output json` against a pulled `alpine:3.19` image, trimmed to
five matches that cover `High`/`Medium`/`Low` severities and the `fixed`, `unknown`, and empty
fix states.

`controller-image.json` is a verbatim scan of a local `ko`-built controller image (Wolfi base,
no package matches), which is the shape the image regression job compares in production.
