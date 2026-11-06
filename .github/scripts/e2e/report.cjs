const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {repo, validateSource, verifyFiles} = require('./gate.cjs');
const checkName = source => `Manual E2E / ${source.preset}`;
const externalID = source => `e2e:${source.run}:1:${source.preset}:pr:${source.pr}`;
const runURL = source => `https://github.com/${repo.owner}/${repo.repo}/actions/runs/${source.run}`;
function readSummary(env, source) {
  verifyFiles(path.join(env.RUNNER_TEMP, 'report'), JSON.parse(fs.readFileSync(path.join(env.RUNNER_TEMP, 'summary-size.json'))));
  const data = JSON.parse(fs.readFileSync(path.join(env.RUNNER_TEMP, 'report/summary.json/summary.json')));
  assert.deepEqual(Object.keys(data).sort(), ['source_sha', 'preset', 'executed', 'passed', 'failed', 'skipped', 'duration', 'tests_ok', 'cleanup_ok'].sort());
  assert.equal(data.source_sha, source.sha);
  assert.equal(data.preset, source.preset);
  for (const key of ['executed', 'passed', 'failed', 'skipped']) assert(Number.isSafeInteger(data[key]) && data[key] >= 0 && data[key] <= 10000);
  assert.equal(data.executed, data.passed + data.failed);
  assert(data.executed + data.skipped <= 10000);
  assert(typeof data.duration === 'number' && Number.isFinite(data.duration) && data.duration >= 0 && data.duration <= 86400);
  assert(typeof data.tests_ok === 'boolean' && typeof data.cleanup_ok === 'boolean');
  if (data.tests_ok) assert(data.executed > 0 && data.failed === 0);
  return data;
}
function verdict(env, data) {
  const outcomes = [env.BUILD_STATUS, env.RUNTIME_STATUS, env.PROJECT_STATUS, env.SUMMARY_UPLOAD_STATUS, env.NATIVE_UPLOAD_STATUS, env.DOWNLOAD_STATUS];
  if (outcomes.includes('cancelled')) return 'cancelled';
  return outcomes.every(status => status === 'success') && data?.tests_ok && data.cleanup_ok && data.executed > 0 ? 'success' : 'failure';
}
async function pages(fetch, args, field) {
  const result = [];
  for (let page = 1; page <= 10; page++) {
    const {data} = await fetch({...args, per_page: 100, page});
    const items = field ? data[field] : data;
    result.push(...items);
    if (items.length < 100) return result;
  }
  throw new Error('publication history exceeds bound');
}
async function publishComment(github, source, body) {
  const {data: pr} = await github.rest.pulls.get({...repo, pull_number: source.pr});
  if (pr.head.sha !== source.sha) return 'stale-sha';
  const checks = await pages(github.rest.checks.listForRef, {...repo, ref: source.sha, check_name: checkName(source), filter: 'all'}, 'check_runs');
  const checkPattern = new RegExp(`^e2e:([1-9][0-9]*):1:${source.preset}:pr:${source.pr}$`);
  if (checks.some(check => check.app?.slug === 'github-actions' && check.head_sha === source.sha &&
      Number(check.external_id?.match(checkPattern)?.[1]) > source.run)) return 'superseded';
  const comments = await pages(github.rest.issues.listComments, {...repo, issue_number: source.pr});
  const marker = new RegExp(`^<!-- karpenter-e2e:${source.preset} run=([1-9][0-9]*) sha=[a-f0-9]{40} -->\\n`);
  const own = comments.filter(comment => comment.user?.type === 'Bot' && comment.user.login === 'github-actions[bot]' && marker.test(comment.body));
  if (own.some(comment => Number(comment.body.match(marker)[1]) > source.run)) return 'superseded';
  own.sort((a, b) => Number(b.body.match(marker)[1]) - Number(a.body.match(marker)[1]) || b.id - a.id);
  if (own.length) await github.rest.issues.updateComment({...repo, comment_id: own[0].id, body});
  else await github.rest.issues.createComment({...repo, issue_number: source.pr, body});
  return 'updated';
}
async function run({github, core}, mode, env = process.env) {
  const source = JSON.parse(env.SOURCE);
  validateSource(source, env);
  if (mode === 'pending') {
    const {data: check} = await github.rest.checks.create({...repo, name: checkName(source), head_sha: source.sha,
      external_id: externalID(source), status: 'in_progress', details_url: runURL(source),
      output: {title: 'Awaiting E2E evidence', summary: `Captured ${source.sha}; ${source.preset}; 20 workers. Missing evidence cannot pass.`}});
    assert(Number.isSafeInteger(check.id) && check.id > 0);
    core.setOutput('check', String(check.id));
    return;
  }
  assert.equal(mode, 'publish');
  let data;
  try { data = readSummary(env, source); } catch { /* Missing/corrupt evidence is a failure. */ }
  const conclusion = verdict(env, data);
  const counts = data ? `${data.executed} executed · ${data.passed} passed · ${data.failed} failed · ${data.skipped} skipped · ${data.duration.toFixed(1)}s` : 'Native evidence unavailable or invalid';
  const body = `<!-- karpenter-e2e:${source.preset} run=${source.run} sha=${source.sha} -->\n` +
    `**E2E ${source.preset}: ${conclusion}** · 20 workers\n\n` +
    `Source \`${source.sha}\` · trusted \`${source.workflow_sha}\` · actor ${source.actor} (${source.actor_id})\n\n` +
    `${counts}\n\nCleanup: ${data ? (data.cleanup_ok ? 'verified' : 'failed') : 'unavailable'}. ` +
    `Build/runtime/projection/uploads/download: ${[env.BUILD_STATUS, env.RUNTIME_STATUS, env.PROJECT_STATUS, env.SUMMARY_UPLOAD_STATUS, env.NATIVE_UPLOAD_STATUS, env.DOWNLOAD_STATUS].map(s => ['success', 'failure', 'cancelled', 'skipped'].includes(s) ? s : 'unavailable').join('/')}\n\n` +
    `[Run and sanitized native-format artifacts](${runURL(source)}) · raw logs private/unavailable.`;
  let checkOK = false;
  try {
    assert(/^[1-9][0-9]*$/.test(env.CHECK_ID) && Number.isSafeInteger(Number(env.CHECK_ID)));
    const {data: check} = await github.rest.checks.get({...repo, check_run_id: Number(env.CHECK_ID)});
    assert.equal(check.id, Number(env.CHECK_ID));
    assert.equal(check.head_sha, source.sha);
    assert.equal(check.external_id, externalID(source));
    assert.equal(check.app.slug, 'github-actions');
    await github.rest.checks.update({...repo, check_run_id: check.id, status: 'completed', conclusion,
      output: {title: `${checkName(source)}: ${conclusion}`, summary: body}});
    checkOK = true;
  } catch { core.error('Own frozen-SHA check could not be finalized; manual reconciliation required'); }
  // Never make check finalization depend on comment reads, ordering or permissions.
  let comment = 'failed';
  try { comment = await publishComment(github, source, body); } catch { core.warning('PR comment unavailable'); }
  await core.summary.addRaw(`${body}\n\nCheck finalized: ${checkOK}. Comment: ${comment}.\n`).write();
  if (!checkOK || comment === 'failed') core.setFailed('E2E publication incomplete');
}
module.exports = {run, readSummary, verdict, publishComment, externalID};
