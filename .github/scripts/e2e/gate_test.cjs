const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {files, metadata, verifyFiles, authorize, configuration} = require('./gate.cjs');
const source = {run: 12, workflow_sha: 'a'.repeat(40)};
const ids = files.map((_, i) => String(i + 1));
const digests = files.map(() => 'b'.repeat(64));
const artifacts = files.map((name, index) => ({id: index + 1, name, digest: `sha256:${digests[index]}`, size_in_bytes: 10,
  expired: false, workflow_run: {id: 12, repository_id: 863038341, head_repository_id: 863038341, head_sha: source.workflow_sha, head_branch: 'main'}}));
function api(entries) {
  return {rest: {actions: {getArtifact: async ({artifact_id}) => ({data: entries.find(a => a.id === artifact_id)})}}};
}
test('five raw IDs require complete same-run provenance, digests and bounded sizes', async () => {
  assert.equal((await metadata(api(artifacts), source, ids, digests)).length, 5);
  for (const mutate of [
    entries => entries.pop(), entries => { entries[0].digest = ''; },
    entries => { entries[0].size_in_bytes = 0; }, entries => { entries[1].size_in_bytes = 2 ** 30; },
    entries => { entries[0].workflow_run.head_sha = 'c'.repeat(40); },
    entries => { entries[0].workflow_run.repository_id = 1; },
    entries => { entries[0].workflow_run.id = 13; }, entries => { entries[0].name = '../bad'; },
    entries => { entries[0].expired = true; }
  ]) {
    const entries = structuredClone(artifacts);
    mutate(entries);
    await assert.rejects(metadata(api(entries), source, ids, digests));
  }
  await assert.rejects(metadata(api(artifacts), source, ['1', '1', '3', '4', '5'], digests));
  await assert.rejects(metadata(api(artifacts), source, ids.slice(1), digests));
  await assert.rejects(metadata(api(artifacts), source, ids, digests.map(() => '')));
});
test('post-download check rejects partial IDs, sizes, links, unexpected paths and stale directories', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-files-'));
  const entries = files.map(name => ({name, size: 1}));
  try {
    for (const {name} of entries) { fs.mkdirSync(path.join(root, name)); fs.writeFileSync(path.join(root, name, name), 'x'); }
    verifyFiles(root, entries);
    const file = path.join(root, files[0], files[0]);
    fs.writeFileSync(file, 'xx');
    assert.throws(() => verifyFiles(root, entries));
    fs.unlinkSync(file);
    assert.throws(() => verifyFiles(root, entries));
    fs.symlinkSync(path.join(root, files[1], files[1]), file);
    assert.throws(() => verifyFiles(root, entries));
    fs.unlinkSync(file); fs.writeFileSync(file, 'x');
    fs.writeFileSync(path.join(root, 'unexpected'), 'x');
    assert.throws(() => verifyFiles(root, entries));
  } finally { fs.rmSync(root, {recursive: true, force: true}); }
});
test('numeric repository/actor/main/event/rerun gate and disabled configuration fail closed', async () => {
  const env = {GITHUB_REPOSITORY: 'cloudpilot-ai/karpenter-provider-gcp', GITHUB_REPOSITORY_ID: '863038341',
    GITHUB_REF: 'refs/heads/main', GITHUB_WORKFLOW_REF: 'cloudpilot-ai/karpenter-provider-gcp/.github/workflows/e2e.yaml@refs/heads/main',
    GITHUB_EVENT_NAME: 'workflow_dispatch', GITHUB_RUN_ATTEMPT: '1', GITHUB_WORKFLOW_SHA: source.workflow_sha,
    GITHUB_RUN_ID: '12', GITHUB_ACTOR_ID: '42', GITHUB_ACTOR: 'maintainer'};
  const upstream = {id: 863038341, full_name: env.GITHUB_REPOSITORY, owner: {id: 166281999}, default_branch: 'main'};
  const actor = {id: 42, login: 'maintainer'};
  const github = {rest: {repos: {get: async () => ({data: upstream}), getCollaboratorPermissionLevel: async () => ({data: {user: actor, permission: 'write'}})},
    actions: {getWorkflowRun: async () => ({data: {id: 12, run_attempt: 1, event: 'workflow_dispatch', path: '.github/workflows/e2e.yaml',
      head_branch: 'main', head_sha: source.workflow_sha, actor, repository: upstream}})}}};
  await authorize(github, env);
  for (const [key, value] of Object.entries({GITHUB_REPOSITORY_ID: '1', GITHUB_REF: 'refs/heads/test', GITHUB_EVENT_NAME: 'pull_request', GITHUB_RUN_ATTEMPT: '2', GITHUB_ACTOR_ID: '43'})) {
    await assert.rejects(authorize(github, {...env, [key]: value}));
  }
  actor.id = 43;
  await assert.rejects(authorize(github, env));
  assert.throws(() => configuration({E2E_CI_ENABLED: 'false'}, 'standard'));
  assert.throws(() => configuration({E2E_CI_ENABLED: 'true', E2E_ENABLED_PRESETS: 'standard'}, 'gpu'));
});
