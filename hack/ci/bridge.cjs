// This bridge is loaded from the workflow snapshot, never from the PR bundle.
const fs = require('node:fs');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
const {capture, capability, readJSON, validateBundle, requireValue, digest, validateResult} = require('./contract.cjs');

async function api(route, options = {}) {
  const response = await fetch(`https://api.github.com${route}`, {
    ...options, headers: {Authorization: `Bearer ${process.env.GH_TOKEN}`, Accept: 'application/vnd.github+json', 'Content-Type': 'application/json', 'X-GitHub-Api-Version': '2022-11-28', ...options.headers},
    signal: AbortSignal.timeout(30000),
  });
  if (!response.ok) throw Error(`GitHub API ${response.status}`);
  return response.json();
}
function output(key, value) {
  requireValue(!String(value).includes('\n'), 'multiline output');
  fs.appendFileSync(process.env.GITHUB_OUTPUT, `${key}=${value}\n`);
}
const commandTimeouts = {capabilities: 2 * 60 * 1000, prepare: 20 * 60 * 1000, run: 45 * 60 * 1000, report: 2 * 60 * 1000};
function invoke(binary, command, args = [], diagnostics = 'diagnostics', timeoutMs = commandTimeouts[command]) {
  requireValue(Number.isSafeInteger(timeoutMs) && timeoutMs > 0, 'invalid tooling deadline');
  const child = spawnSync(binary, [command, ...args], {encoding: 'utf8', maxBuffer: 1024 * 1024, timeout: timeoutMs, killSignal: 'SIGKILL', stdio: ['ignore', 'pipe', 'pipe']});
  fs.mkdirSync(diagnostics, {recursive: true});
  for (const stream of ['stdout', 'stderr']) fs.writeFileSync(path.join(diagnostics, `${command}.${stream}.log`), (child[stream] || '').slice(0, 1024 * 1024));
  if (child.error || child.status !== 0) throw Error(`tooling ${command} failed (${child.status ?? 'process error'})`);
  return child.stdout;
}
async function main(command) {
  if (command === 'capture') {
    const event = readJSON(process.env.GITHUB_EVENT_PATH);
    requireValue(event.issue?.pull_request, 'not a pull request');
    const target = Object.fromEntries(['project', 'region', 'location', 'prefix', 'provider', 'serviceAccount'].map(key => [key, process.env[`TARGET_${key.toUpperCase()}`] || '']));
    const enabled = process.env.MODE === 'standard' && process.env.RUNTIME_ENABLED === 'true';
    if (enabled) requireValue(Object.values(target).every(v => /^[A-Za-z0-9@._:/-]+$/.test(v)), 'missing or invalid runtime configuration');
    const invocation = await capture({toolingRef: process.env.TOOLING_REF, repository: process.env.GITHUB_REPOSITORY, pr: event.issue.number, run: process.env.GITHUB_RUN_ID, attempt: process.env.GITHUB_RUN_ATTEMPT, mode: process.env.MODE, enabled, target}, api);
    output('invocation', JSON.stringify(invocation));
    output('tooling_sha', invocation.tooling?.sha || '');
    output('tested_sha', invocation.tested.sha);
    output('tested_repository', invocation.tested.repository);
    return;
  }
  if (command === 'publish') {
    const {publish} = require('./report.cjs');
    const event = readJSON(process.env.GITHUB_EVENT_PATH);
    requireValue(event.issue?.pull_request, 'not a pull request');
    let invocation = null;
    let result;
    let evidenceError = ['failure', 'cancelled'].includes(process.env.REPORT_OUTCOME) || process.env.REPORT_FAILED === 'true';
    try {
      invocation = process.env.INVOCATION ? JSON.parse(process.env.INVOCATION) : null;
      if (process.env.REPORT_OUTCOME === 'success') result = validateResult(readJSON('report.json'), invocation);
    } catch { evidenceError = true; }
    const report = await publish({invocation, result, evidenceError, jobs: JSON.parse(process.env.JOBS),
      target: {repository: process.env.GITHUB_REPOSITORY, pr: event.issue.number, run: process.env.GITHUB_RUN_ID, attempt: process.env.GITHUB_RUN_ATTEMPT},
      render: markdown => {
        fs.writeFileSync('report.md', markdown);
        fs.writeFileSync(process.env.GITHUB_STEP_SUMMARY, markdown);
      }}, api);
    output('failed', report.failed);
    return;
  }
  const invocation = JSON.parse(process.env.INVOCATION);
  const invocationPath = path.resolve('invocation.json');
  fs.writeFileSync(invocationPath, JSON.stringify(invocation));
  const binary = path.resolve(process.env.RUNNER || 'runner');
  if (command === 'validate') {
    requireValue(digest('bundle/manifest.json') === process.env.EXPECTED_MANIFEST_SHA, 'manifest digest mismatch');
    validateBundle('bundle', invocation);
  } else if (command === 'package') {
    validateResult(readJSON('result.json'), invocation);
    fs.mkdirSync('results', {recursive: true});
    fs.copyFileSync('result.json', 'results/result.json');
    fs.writeFileSync('results/manifest.json', JSON.stringify({version: 1, invocation, files: [{path: 'result.json', sha256: digest('results/result.json')}]}));
    output('manifest_sha', digest('results/manifest.json'));
  } else if (command === 'preflight') {
    output('state', capability(JSON.parse(invoke(binary, 'capabilities')), invocation));
  } else if (command === 'report') {
    let result;
    if (process.env.EXPECTED_MANIFEST_SHA) {
      requireValue(digest('bundle/manifest.json') === process.env.EXPECTED_MANIFEST_SHA, 'manifest digest mismatch');
      const manifest = validateBundle('bundle', invocation);
      requireValue(manifest.files.length === 1 && manifest.files[0].path === 'result.json', 'unexpected report artifact');
      result = validateResult(readJSON('bundle/result.json'), invocation);
    } else {
      requireValue(process.env.PREFLIGHT_STATE !== 'ready', 'missing runtime artifact');
      result = {version: 1, invocation, status: 'not_implemented', executed: 0};
      fs.mkdirSync('bundle', {recursive: true});
      fs.writeFileSync('bundle/result.json', JSON.stringify(result));
    }
    invoke(binary, 'report', ['--invocation', invocationPath, '--bundle', path.resolve('bundle'), '--output', path.resolve('report.json')]);
    const report = validateResult(readJSON('report.json'), invocation);
    requireValue(report.status === result.status && report.executed === result.executed, 'report changed execution outcome');
  } else if (['prepare', 'run'].includes(command)) {
    const bundle = path.resolve('bundle');
    if (command !== 'prepare') validateBundle(bundle, invocation);
    const result = path.resolve('result.json');
    invoke(binary, command, ['--invocation', invocationPath, '--source', path.resolve('source'), '--bundle', bundle, '--output', result]);
    if (command === 'run') {
      const outcome = validateResult(readJSON(result), invocation);
      requireValue(outcome.status !== 'failed', 'tests failed');
    }
    if (command === 'prepare') {
      validateBundle(bundle, invocation);
      output('manifest_sha', digest(path.join(bundle, 'manifest.json')));
    }
  } else throw Error('unknown bridge command');
}
if (require.main === module) main(process.argv[2]).catch(error => { console.error(error.message); process.exitCode = 1; });
module.exports = {api, invoke, main};
