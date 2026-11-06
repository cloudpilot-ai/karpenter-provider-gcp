const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {run, verdict, publishComment, externalID, readSummary} = require('./report.cjs');
const source = {sha: 'a'.repeat(40), pr: 708, preset: 'standard', run: 12, workflow_sha: 'b'.repeat(40),
  actor: 'maintainer', actor_id: 42, head_repository_id: 1, head_repository: 'fork/repo'};
const env = {SOURCE: JSON.stringify(source), GITHUB_RUN_ID: '12', GITHUB_WORKFLOW_SHA: source.workflow_sha,
  GITHUB_ACTOR: 'maintainer', GITHUB_ACTOR_ID: '42', CHECK_ID: '123', RUNNER_TEMP: '/no-evidence'};
test('independent job/cleanup/upload/cancel/missing evidence fences override test counts', () => {
  const statuses = Object.fromEntries(['BUILD_STATUS', 'RUNTIME_STATUS', 'PROJECT_STATUS', 'SUMMARY_UPLOAD_STATUS', 'NATIVE_UPLOAD_STATUS', 'DOWNLOAD_STATUS'].map(key => [key, 'success']));
  const data = {tests_ok: true, cleanup_ok: true, executed: 1};
  assert.equal(verdict(statuses, data), 'success');
  assert.equal(verdict(statuses, undefined), 'failure');
  assert.equal(verdict(statuses, {...data, executed: 0}), 'failure');
  assert.equal(verdict(statuses, {...data, cleanup_ok: false}), 'failure');
  for (const key of Object.keys(statuses)) {
    assert.equal(verdict({...statuses, [key]: 'failure'}, data), 'failure');
    assert.equal(verdict({...statuses, [key]: 'cancelled'}, data), 'cancelled');
  }
});
test('own frozen-SHA check completes before any failing comment reads; raw API errors never escape', async () => {
  const events = [];
  const github = {rest: {checks: {get: async () => ({data: {id: 123, head_sha: source.sha, external_id: externalID(source), app: {slug: 'github-actions'}}}),
    update: async args => { events.push('finalized'); assert.equal(args.conclusion, 'failure'); }},
    pulls: {get: async () => { events.push('comment'); throw new Error('SECRET'); }}}};
  const core = {error: () => {}, warning: message => assert(!message.includes('SECRET')), setFailed: () => {},
    summary: {addRaw: text => { assert(!text.includes('SECRET')); return {write: async () => {}}; }}};
  await run({github, core}, 'publish', env);
  assert.deepEqual(events, ['finalized', 'comment']);
});
test('same PR/preset newer pending checks and bot-only comments fence stale publication', async () => {
  let comments = [], checks = [], writes = [];
  const github = {rest: {pulls: {get: async () => ({data: {head: {sha: source.sha}}})},
    checks: {listForRef: async () => ({data: {check_runs: checks}})},
    issues: {listComments: async () => ({data: comments}), createComment: async args => writes.push(args), updateComment: async args => writes.push(args)}}};
  checks = [{app: {slug: 'github-actions'}, head_sha: source.sha, external_id: externalID({...source, run: 13})}];
  assert.equal(await publishComment(github, source, 'safe'), 'superseded');
  checks = [];
  comments = [{id: 1, user: {type: 'User', login: 'github-actions[bot]'}, body: `<!-- karpenter-e2e:standard run=14 sha=${source.sha} -->\nforged`}];
  assert.equal(await publishComment(github, source, 'safe'), 'updated');
  assert(!('comment_id' in writes[0]));
  comments[0].user.type = 'Bot';
  assert.equal(await publishComment(github, source, 'safe'), 'superseded');
  assert.equal(writes.length, 1);
});

test('raw summary exact-file proof and bounded independent source/count validation', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-summary-'));
  try {
    fs.mkdirSync(path.join(root, 'report/summary.json'), {recursive: true});
    const file = path.join(root, 'report/summary.json/summary.json');
    const data = {source_sha: source.sha, preset: source.preset, executed: 1, passed: 1, failed: 0,
      skipped: 0, duration: 1, tests_ok: true, cleanup_ok: true};
    const write = value => {
      fs.writeFileSync(file, JSON.stringify(value));
      fs.writeFileSync(path.join(root, 'summary-size.json'), JSON.stringify([{name: 'summary.json', size: fs.statSync(file).size}]));
    };
    write(data);
    assert.deepEqual(readSummary({RUNNER_TEMP: root}, source), data);
    for (const invalid of [{...data, source_sha: 'c'.repeat(40)}, {...data, executed: 0},
      {...data, passed: true}, {...data, duration: -1}, {...data, skipped: 10001}, {...data, raw: 'SECRET'}]) {
      write(invalid);
      assert.throws(() => readSummary({RUNNER_TEMP: root}, source));
    }
    write(data);
    fs.appendFileSync(file, 'x');
    assert.throws(() => readSummary({RUNNER_TEMP: root}, source));
  } finally { fs.rmSync(root, {recursive: true, force: true}); }
});
