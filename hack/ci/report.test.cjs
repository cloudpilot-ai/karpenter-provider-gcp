const {test} = require('node:test');
const assert = require('node:assert/strict');
const {publish} = require('./report.cjs');
const invocation = {version: 1, repository: 'dm3ch/provider', pr: 510, run: '123', attempt: '1', mode: 'standard', tested: {repository: 'fork/provider', sha: 'b'.repeat(40)}, tooling: {repository: 'cloudpilot-ai/karpenter-provider-gcp', ref: 'bootstrap', sha: 'a'.repeat(40)}};
const jobs = {gate: {result: 'success'}, 'tooling-preflight': {result: 'success', outputs: {state: 'not_implemented'}}, build: {result: 'skipped'}, runtime: {result: 'skipped'}};
const target = {repository: invocation.repository, pr: 510, run: '123', attempt: '1'};
test('neutral bootstrap publishes checks on frozen tested SHA and uses triggering fork', async () => {
  const calls = [];
  const result = await publish({invocation, target, jobs}, async (route, options) => { calls.push({route, body: JSON.parse(options.body)}); });
  assert.equal(result.failed, false);
  assert.deepEqual(calls.map(c => c.body.conclusion), ['success', 'neutral']);
  assert.ok(calls.every(c => c.route === '/repos/dm3ch/provider/check-runs' && c.body.head_sha === invocation.tested.sha));
  assert.match(result.markdown, /not_implemented/);
  assert.ok(result.markdown.includes(invocation.tooling.sha));
  assert.ok(result.markdown.includes(invocation.tested.sha));
  assert.match(result.markdown, /actions\/runs\/123/);
});
test('job, artifact and zero-test failures cannot be masked by a successful report', async () => {
  const {validateResult} = require('./contract.cjs');
  assert.throws(() => validateResult({version: 1, invocation, status: 'passed', executed: 0}, invocation), /invalid execution count/);
  for (const failure of [{...jobs, build: {result: 'failure'}}, {...jobs, runtime: {result: 'failure'}}, {...jobs, runtime: {result: 'cancelled'}}]) {
    const calls = [];
    const report = await publish({invocation, target, jobs: failure, result: {version: 1, invocation, status: 'passed', executed: 2}}, async (_, options) => calls.push(JSON.parse(options.body)));
    assert.equal(report.failed, true);
    assert.equal(calls[0].conclusion, 'failure');
    assert.notEqual(calls[1].conclusion, 'success');
  }
  assert.equal((await publish({invocation, target, jobs, evidenceError: true}, async () => {})).failed, true);
  const result = {version: 1, invocation, status: 'failed', executed: 1};
  assert.equal((await publish({invocation, target, jobs, result}, async () => {})).failed, true);
});
test('capture failure and unsupported modes report safely without tooling identity', async () => {
  const calls = [];
  const report = await publish({target, jobs: {...jobs, gate: {result: 'failure'}}, invocation: null}, async route => calls.push(route));
  assert.equal(report.failed, true);
  assert.match(report.markdown, /capture failed/);
  assert.deepEqual(calls, []);
  const unsupported = await publish({target, invocation: {...invocation, tooling: null, mode: 'gpu'}, jobs: {...jobs, 'tooling-preflight': {result: 'skipped'}}}, async () => {});
  assert.equal(unsupported.failed, false);
  assert.match(unsupported.markdown, /not resolved/);
  await assert.rejects(publish({target, invocation: {...invocation, pr: 999}, jobs}, async () => {}), /identity mismatch/);
});
test('artifact identities cannot redirect checks and missing ready-run results fail closed', async () => {
  const {validateResult} = require('./contract.cjs');
  assert.throws(() => validateResult({version: 1, invocation: {...invocation, repository: 'cloudpilot-ai/karpenter-provider-gcp', pr: 999}, status: 'passed', executed: 1}, invocation), /identity mismatch/);
  const ready = {...jobs, 'tooling-preflight': {result: 'success', outputs: {state: 'ready'}}, runtime: {result: 'success'}};
  const report = await publish({target, invocation, jobs: ready}, async () => {});
  assert.equal(report.failed, true);
});
test('failed check publication replaces success comments with truthful failure and propagates the error', async () => {
  let markdown;
  await assert.rejects(publish({target, invocation, jobs, render: value => { markdown = value; }}, async () => { throw Error('GitHub API 500'); }), /GitHub API 500/);
  assert.match(markdown, /Harness validation: \*\*failure\*\*/);
  assert.match(markdown, /Check publication failed/);
  assert.doesNotMatch(markdown, /Harness validation: \*\*success\*\*/);
});
