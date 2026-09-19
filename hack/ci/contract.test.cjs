const {test} = require('node:test');
const assert = require('node:assert/strict');
const {capture} = require('./contract.cjs');
const shaA = 'a'.repeat(40), shaB = 'b'.repeat(40);
const config = {toolingRef: 'e2e-ci-bootstrap', repository: 'dm3ch/karpenter-provider-gcp', pr: 510, run: '123', attempt: '1', mode: 'standard', enabled: false, target: {}};
test('captures exact upstream branch and independent PR source once per invocation', async () => {
  let tip = shaA;
  const api = async path => {
    if (path === '/repos/cloudpilot-ai/karpenter-provider-gcp/git/ref/heads/e2e-ci-bootstrap') return {ref: 'refs/heads/e2e-ci-bootstrap', object: {type: 'commit', sha: tip}};
    if (path === '/repos/dm3ch/karpenter-provider-gcp/pulls/510') return {head: {sha: shaB, repo: {full_name: 'contributor/provider'}}};
    throw Error(path);
  };
  const first = await capture(config, api);
  tip = shaB;
  const second = await capture(config, api);
  assert.equal(first.tooling.sha, shaA);
  assert.equal(second.tooling.sha, shaB);
  assert.deepEqual(first.tested, {repository: 'contributor/provider', sha: shaB});
  assert.equal(first.repository, config.repository);
});
module.exports = {config, shaA, shaB};
const {capability} = require('./contract.cjs');
test('preflight fails closed and distinguishes unsupported from disabled', () => {
  const caps = {version: 1, commands: ['capabilities', 'prepare', 'run', 'report'], modes: {standard: 'not_implemented'}};
  assert.equal(capability(caps, {...config, enabled: true}), 'not_implemented');
  assert.equal(capability({...caps, modes: {standard: 'supported'}}, config), 'disabled');
  assert.equal(capability({...caps, modes: {standard: 'supported'}}, {...config, enabled: true}), 'ready');
  for (const invalid of [null, {}, {...caps, version: 2}, {...caps, commands: ['run']}, {...caps, modes: {}}, {...caps, modes: {standard: 'maybe'}}]) {
    assert.throws(() => capability(invalid, config));
  }
});
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const {validateBundle} = require('./contract.cjs');
test('bundle validation binds same-run identities and digests and rejects unsafe paths', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-contract-'));
  try {
    fs.writeFileSync(path.join(dir, 'controller'), 'code under test');
    const invocation = {...config, tested: {repository: 'fork/repo', sha: shaB}, tooling: {repository: 'cloudpilot-ai/karpenter-provider-gcp', sha: shaA}};
    const manifest = {version: 1, invocation, files: [{path: 'controller', sha256: crypto.createHash('sha256').update('code under test').digest('hex')}]};
    const write = value => fs.writeFileSync(path.join(dir, 'manifest.json'), JSON.stringify(value));
    write(manifest);
    assert.doesNotThrow(() => validateBundle(dir, invocation));
    assert.throws(() => validateBundle(dir, {...invocation, run: '999'}));
    for (const name of ['../escape', '/absolute', 'sub/../controller', 'manifest.json', 'a\\b']) {
      write({...manifest, files: [{...manifest.files[0], path: name}]});
      assert.throws(() => validateBundle(dir, invocation));
    }
    write(manifest);
    fs.writeFileSync(path.join(dir, 'controller'), 'tampered');
    assert.throws(() => validateBundle(dir, invocation));
    fs.unlinkSync(path.join(dir, 'controller'));
    fs.symlinkSync('/etc/hosts', path.join(dir, 'controller'));
    assert.throws(() => validateBundle(dir, invocation));
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
test('missing or misleading source resolutions fail closed', async () => {
  for (const response of [{}, {ref: 'refs/tags/e2e-ci-bootstrap', object: {type: 'commit', sha: shaA}}, {ref: 'refs/heads/e2e-ci-bootstrap', object: {type: 'commit', sha: 'short'}}]) {
    await assert.rejects(capture(config, async () => response));
  }
  await assert.rejects(capture(config, async () => { throw Error('404'); }));
});
test('unsupported modes capture PR identity without tooling or runtime configuration', async () => {
  for (const mode of ['gpu', 'full']) {
    const invocation = await capture({...config, mode, toolingRef: '', enabled: true}, async route => {
      assert.equal(route, '/repos/dm3ch/karpenter-provider-gcp/pulls/510');
      return {head: {sha: shaB, repo: {full_name: 'contributor/provider'}}};
    });
    assert.equal(invocation.tooling, null);
    assert.equal(invocation.enabled, false);
    assert.deepEqual(invocation.target, {});
  }
});
