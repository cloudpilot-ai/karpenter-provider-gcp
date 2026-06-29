# owners-check

`owners-check` is a small helper for GitHub Actions gates that need to know whether an actor is listed in the trusted root `OWNERS` file.

It intentionally checks only root `OWNERS.approvers` from the checked-out default branch. It does not implement full Prow path ownership semantics, nested OWNERS inheritance, `OWNERS_ALIASES`, or changed-file approval requirements. That is deliberate for `/e2e`: the trigger should be limited to repo-level e2e operators, not every future path-specific approver.

## Inputs

Environment variables:

| Variable | Required | Meaning |
|----------|----------|---------|
| `ACTOR` | Yes | GitHub login to check. |
| `OWNERS_PATH` | No | Path to the OWNERS file. Defaults to `OWNERS`. |

## Outputs

When `GITHUB_OUTPUT` is set, the tool writes:

| Output | Meaning |
|--------|---------|
| `allowed` | `true` if `ACTOR` is listed under root `approvers`, otherwise `false`. |
| `actor` | Echoes the checked actor. |

Without `GITHUB_OUTPUT`, outputs are printed as `name=value` lines for local debugging.

## Local tests

Run from the existing `hack/tools` module:

```bash
cd hack/tools
go test ./owners-check/...
```

The helper uses only the Go standard library, so it does not need a separate `go.mod` or Dependabot entry. Dependabot already tracks the parent `/hack/tools` module.
