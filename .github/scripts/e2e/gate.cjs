const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const repo = {owner: 'cloudpilot-ai', repo: 'karpenter-provider-gcp'};
const repository = `${repo.owner}/${repo.repo}`;
const repositoryID = 863038341;
const ownerID = 166281999;
const workflow = '.github/workflows/e2e.yaml';
const files = ['controller-image.tar', 'controller-chart.tgz', 'crd-chart.tgz', 'e2e.test', 'ginkgo'];
const limits = [1 << 30, 16 << 20, 16 << 20, 512 << 20, 128 << 20];
const targetKeys = ['PROJECT_ID', 'LOCATION', 'REGION', 'PREFIX', 'CLUSTER_NAME', 'IMAGE_REPOSITORY',
  'CONTROLLER_SERVICE_ACCOUNT', 'NODE_SERVICE_ACCOUNT', 'PODS_RANGE_NAME', 'SMALL_PODS_RANGE_NAME',
  'LARGE_PODS_RANGE_NAME', 'RESOURCE_MANAGER_TAG_KEY'].map(key => `E2E_${key}`);
const positive = value => /^[1-9][0-9]*$/.test(String(value)) && Number.isSafeInteger(Number(value));
const sha = value => typeof value === 'string' && /^[a-f0-9]{40}$/.test(value);
const preset = value => ['standard', 'gpu', 'full'].includes(value);
function configuration(env, selection) {
  assert.equal(env.E2E_CI_ENABLED, 'true');
  const presets = env.E2E_ENABLED_PRESETS.split(',').map(p => p.trim());
  assert(presets.every(preset) && presets.includes(selection));
  const target = JSON.parse(env.E2E_TARGET_CONFIG);
  assert.deepEqual(Object.keys(target).sort(), [...targetKeys].sort());
  for (const value of Object.values(target)) assert(typeof value === 'string' && /^[a-zA-Z0-9@/._-]{1,256}$/.test(value));
  const provider = env.E2E_WIF_PROVIDER;
  assert(/^projects\/[0-9]+\/locations\/global\/workloadIdentityPools\/[a-z0-9-]+\/providers\/github$/.test(provider));
  assert.equal(env.E2E_WIF_AUDIENCE, `https://iam.googleapis.com/${provider}`);
  assert.equal(env.E2E_RUNTIME_SERVICE_ACCOUNT, `${target.E2E_PREFIX}-ci-sa@${target.E2E_PROJECT_ID}.iam.gserviceaccount.com`);
  return {provider, audience: env.E2E_WIF_AUDIENCE, service_account: env.E2E_RUNTIME_SERVICE_ACCOUNT, target};
}
function validateSource(source, env) {
  assert(sha(source.sha) && positive(source.pr) && preset(source.preset));
  assert(positive(source.head_repository_id) && /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(source.head_repository));
  assert.equal(source.run, Number(env.GITHUB_RUN_ID));
  assert.equal(source.workflow_sha, env.GITHUB_WORKFLOW_SHA);
  assert.equal(source.actor, env.GITHUB_ACTOR);
  assert.equal(source.actor_id, Number(env.GITHUB_ACTOR_ID));
}
async function authorize(github, env) {
  assert.equal(env.GITHUB_REPOSITORY, repository);
  assert.equal(env.GITHUB_REPOSITORY_ID, String(repositoryID));
  assert.equal(env.GITHUB_REF, 'refs/heads/main');
  assert.equal(env.GITHUB_WORKFLOW_REF, `${repository}/${workflow}@refs/heads/main`);
  assert.equal(env.GITHUB_EVENT_NAME, 'workflow_dispatch');
  assert.equal(env.GITHUB_RUN_ATTEMPT, '1');
  assert(sha(env.GITHUB_WORKFLOW_SHA) && positive(env.GITHUB_RUN_ID) && positive(env.GITHUB_ACTOR_ID));
  assert(/^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/.test(env.GITHUB_ACTOR));
  const {data: upstream} = await github.rest.repos.get(repo);
  assert.equal(upstream.id, repositoryID);
  assert.equal(upstream.full_name, repository);
  assert.equal(upstream.owner.id, ownerID);
  assert.equal(upstream.default_branch, 'main');
  const {data: run} = await github.rest.actions.getWorkflowRun({...repo, run_id: Number(env.GITHUB_RUN_ID)});
  for (const [key, value] of Object.entries({id: Number(env.GITHUB_RUN_ID), run_attempt: 1, event: 'workflow_dispatch',
    path: workflow, head_branch: 'main', head_sha: env.GITHUB_WORKFLOW_SHA})) assert.equal(run[key], value);
  assert.equal(run.actor.id, Number(env.GITHUB_ACTOR_ID));
  assert.equal(run.actor.login, env.GITHUB_ACTOR);
  assert.equal(run.repository.id, repositoryID);
  assert.equal(run.repository.full_name, repository);
  const {data: permission} = await github.rest.repos.getCollaboratorPermissionLevel({...repo, username: env.GITHUB_ACTOR});
  assert.equal(permission.user.id, Number(env.GITHUB_ACTOR_ID));
  assert.equal(permission.user.login, env.GITHUB_ACTOR);
  assert(['write', 'maintain', 'admin'].includes(permission.permission));
}
async function metadata(github, source, ids, digests, names = files, caps = limits) {
  assert.equal(ids.length, names.length);
  assert.equal(digests.length, names.length);
  assert(ids.every(positive) && new Set(ids.map(Number)).size === names.length);
  let total = 0;
  const result = [];
  for (let index = 0; index < names.length; index++) {
    assert(/^[a-f0-9]{64}$/.test(digests[index]));
    const {data: artifact} = await github.rest.actions.getArtifact({...repo, artifact_id: Number(ids[index])});
    assert.equal(artifact.id, Number(ids[index]));
    assert.equal(artifact.name, names[index]);
    assert.equal(artifact.expired, false);
    assert.equal(artifact.digest, `sha256:${digests[index]}`);
    assert(Number.isSafeInteger(artifact.size_in_bytes) && artifact.size_in_bytes > 0 && artifact.size_in_bytes <= caps[index]);
    const run = artifact.workflow_run;
    for (const [key, value] of Object.entries({id: source.run, repository_id: repositoryID, head_repository_id: repositoryID,
      head_sha: source.workflow_sha, head_branch: 'main'})) assert.equal(run[key], value);
    total += artifact.size_in_bytes;
    result.push({name: names[index], size: artifact.size_in_bytes});
  }
  assert(total <= 2 ** 31);
  return result;
}
function verifyFiles(root, entries) {
  assert(fs.lstatSync(root).isDirectory() && !fs.lstatSync(root).isSymbolicLink());
  assert.deepEqual(fs.readdirSync(root).sort(), entries.map(entry => entry.name).sort());
  for (const entry of entries) {
    const directory = path.join(root, entry.name);
    assert(fs.lstatSync(directory).isDirectory() && !fs.lstatSync(directory).isSymbolicLink());
    assert.deepEqual(fs.readdirSync(directory), [entry.name]);
    const info = fs.lstatSync(path.join(directory, entry.name));
    assert(info.isFile() && !info.isSymbolicLink() && info.size === entry.size && info.nlink === 1);
  }
}
async function run({github, core}, mode, env = process.env) {
  if (mode === 'capture' || mode === 'recheck') {
    await authorize(github, env);
    if (mode === 'capture') {
      assert(positive(env.PR_NUMBER) && preset(env.PRESET));
      const config = configuration(env, env.PRESET);
      const {data: pr} = await github.rest.pulls.get({...repo, pull_number: Number(env.PR_NUMBER)});
      assert.equal(pr.number, Number(env.PR_NUMBER));
      assert.equal(pr.state, 'open');
      assert.equal(pr.base.ref, 'main');
      assert.equal(pr.base.repo.id, repositoryID);
      assert.equal(pr.base.repo.full_name, repository);
      const source = {sha: pr.head.sha, pr: pr.number, preset: env.PRESET, head_repository: pr.head.repo.full_name,
        head_repository_id: pr.head.repo.id, run: Number(env.GITHUB_RUN_ID), workflow_sha: env.GITHUB_WORKFLOW_SHA,
        actor: env.GITHUB_ACTOR, actor_id: Number(env.GITHUB_ACTOR_ID)};
      validateSource(source, env);
      core.setOutput('source', JSON.stringify(source));
      core.setOutput('config', JSON.stringify(config));
      for (const key of ['sha', 'pr', 'preset']) core.setOutput(key, source[key]);
      core.setOutput('target', `e2e-${config.target.E2E_PROJECT_ID}-${config.target.E2E_LOCATION}-${config.target.E2E_CLUSTER_NAME}`);
    } else {
      const source = JSON.parse(env.SOURCE);
      validateSource(source, env);
      const config = configuration(env, source.preset);
      assert.deepEqual(config, JSON.parse(env.CONFIG));
      for (const [key, value] of Object.entries(config.target)) core.exportVariable(key, value);
      const entries = await metadata(github, source, JSON.parse(env.ARTIFACT_IDS), JSON.parse(env.ARTIFACT_DIGESTS));
      fs.writeFileSync(path.join(env.RUNNER_TEMP, 'artifact-sizes.json'), JSON.stringify(entries), {flag: 'wx'});
      fs.mkdirSync(path.join(env.RUNNER_TEMP, 'prepared'), {mode: 0o700});
    }
  } else if (mode === 'files') {
    verifyFiles(path.join(env.RUNNER_TEMP, 'prepared'), JSON.parse(fs.readFileSync(path.join(env.RUNNER_TEMP, 'artifact-sizes.json'))));
  } else if (mode === 'summary') {
    const source = JSON.parse(env.SOURCE);
    validateSource(source, env);
    const entries = await metadata(github, source, [env.SUMMARY_ID], [env.SUMMARY_DIGEST], ['summary.json'], [4096]);
    fs.writeFileSync(path.join(env.RUNNER_TEMP, 'summary-size.json'), JSON.stringify(entries), {flag: 'wx'});
    fs.mkdirSync(path.join(env.RUNNER_TEMP, 'report'), {mode: 0o700});
  } else throw new Error('unknown gate mode');
}
module.exports = {run, repo, files, metadata, verifyFiles, validateSource, configuration, authorize, sha};
