# Workflow dependency maintenance

Action dependencies are locked in `.github/workflows/actions.lock`, including
transitive actions. The lockfile determines which commits run; readable tags or
branches in workflow files do not advance those commits automatically.

After adding a workflow or changing an action reference, use the same generator
version as presubmit:

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

The lockfile covers action code, not tools downloaded by actions, container
images, or job-level reusable-workflow calls. Repository policy enforcement is a
separate settings decision; adding this lockfile does not change those settings.

See [GitHub's lockfile documentation](https://docs.github.com/en/actions/concepts/security/workflow-dependency-lockfiles).
