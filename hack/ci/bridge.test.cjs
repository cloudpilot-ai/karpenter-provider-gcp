const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
test('public preflight bridge reads tooling capabilities and never runs prepare on unsupported or invalid input', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-bridge-'));
  try {
    const runner = path.join(dir, 'runner');
    const output = path.join(dir, 'output');
    const capabilities = {version: 1, commands: ['capabilities', 'prepare', 'run', 'report'], modes: {standard: 'not_implemented'}};
    for (const [value, success] of [[capabilities, true], [{version: 2}, false], [null, false]]) {
      fs.writeFileSync(output, '');
      fs.writeFileSync(runner, `#!/usr/bin/env node\nif(process.argv[2] !== 'capabilities') process.exit(99);\nconsole.log(${JSON.stringify(JSON.stringify(value))});\n`, {mode: 0o700});
      const result = spawnSync(process.execPath, [path.resolve(__dirname, 'bridge.cjs'), 'preflight'], {cwd: dir, env: {...process.env, RUNNER: runner, GITHUB_OUTPUT: output, INVOCATION: JSON.stringify({version: 1, mode: 'standard', enabled: true})}});
      assert.equal(result.status, success ? 0 : 1);
      assert.equal(fs.readFileSync(output, 'utf8'), success ? 'state=not_implemented\n' : '');
    }
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
test('failed tooling preserves bounded diagnostics and remains a failure', () => {
  const {invoke} = require('./bridge.cjs');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-diagnostics-'));
  try {
    const runner = path.join(dir, 'runner');
    fs.writeFileSync(runner, '#!/usr/bin/env node\nconsole.log("partial progress"); console.error("controlled failure"); process.exit(7);', {mode: 0o700});
    assert.throws(() => invoke(runner, 'run', [], dir), /failed/);
    assert.match(fs.readFileSync(path.join(dir, 'run.stdout.log'), 'utf8'), /partial progress/);
    assert.match(fs.readFileSync(path.join(dir, 'run.stderr.log'), 'utf8'), /controlled failure/);
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
test('tooling deadline forcibly terminates a stalled process and retains partial diagnostics', () => {
  const {invoke} = require('./bridge.cjs');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-timeout-'));
  try {
    const runner = path.join(dir, 'runner');
    fs.writeFileSync(runner, '#!/usr/bin/env node\nprocess.on("SIGTERM", () => {}); console.log("partial progress"); setTimeout(() => process.exit(0), 3000);', {mode: 0o700});
    assert.throws(() => invoke(runner, 'capabilities', [], dir, 1000), /failed/);
    assert.match(fs.readFileSync(path.join(dir, 'capabilities.stdout.log'), 'utf8'), /partial progress/);
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
test('tooling output overflow fails and retains only bounded diagnostics', () => {
  const {invoke} = require('./bridge.cjs');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-overflow-'));
  try {
    const runner = path.join(dir, 'runner');
    fs.writeFileSync(runner, '#!/usr/bin/env node\nprocess.stdout.write(Buffer.alloc(2 * 1024 * 1024, "x"));', {mode: 0o700});
    assert.throws(() => invoke(runner, 'run', [], dir), /failed/);
    const bytes = fs.statSync(path.join(dir, 'run.stdout.log')).size;
    assert.ok(bytes > 0 && bytes <= 1024 * 1024);
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
test('runtime handoff rejects a changed manifest before execution', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-handoff-'));
  try {
    fs.mkdirSync(path.join(dir, 'bundle'));
    fs.writeFileSync(path.join(dir, 'bundle', 'manifest.json'), '{}');
    const result = spawnSync(process.execPath, [path.resolve(__dirname, 'bridge.cjs'), 'validate'], {cwd: dir, encoding: 'utf8', env: {...process.env, INVOCATION: '{}', EXPECTED_MANIFEST_SHA: 'a'.repeat(64)}});
    assert.equal(result.status, 1);
    assert.match(result.stderr, /manifest digest mismatch/);
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
test('publication CLI handles accepted requests whose capture failed without an invocation or SHA', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-publish-'));
  try {
    fs.writeFileSync(path.join(dir, 'event.json'), JSON.stringify({issue: {number: 510, pull_request: {}}}));
    const result = spawnSync(process.execPath, [path.resolve(__dirname, 'bridge.cjs'), 'publish'], {cwd: dir, encoding: 'utf8', env: {...process.env, INVOCATION: '', JOBS: JSON.stringify({gate: {result: 'failure'}}), GITHUB_EVENT_PATH: path.join(dir, 'event.json'), GITHUB_OUTPUT: path.join(dir, 'output'), GITHUB_STEP_SUMMARY: path.join(dir, 'summary'), GITHUB_REPOSITORY: 'dm3ch/provider', GITHUB_RUN_ID: '123', GITHUB_RUN_ATTEMPT: '1'}});
    assert.equal(result.status, 0, result.stderr);
    assert.match(fs.readFileSync(path.join(dir, 'report.md'), 'utf8'), /capture failed/);
    assert.equal(fs.readFileSync(path.join(dir, 'output'), 'utf8'), 'failed=true\n');
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
test('publication treats reporter cancellation as failure without changing skipped runtime to success', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-cancel-'));
  try {
    const invocation = {version: 1, repository: 'dm3ch/provider', pr: 510, run: '123', attempt: '1', mode: 'standard', tested: {repository: 'fork/provider', sha: 'b'.repeat(40)}, tooling: {sha: 'a'.repeat(40)}};
    const jobs = {gate: {result: 'success'}, 'tooling-preflight': {result: 'success', outputs: {state: 'not_implemented'}}, build: {result: 'skipped'}, runtime: {result: 'skipped'}};
    fs.writeFileSync(path.join(dir, 'event.json'), JSON.stringify({issue: {number: 510, pull_request: {}}}));
    fs.writeFileSync(path.join(dir, 'report.json'), JSON.stringify({version: 1, invocation, status: 'not_implemented', executed: 0}));
    fs.writeFileSync(path.join(dir, 'fake-api.cjs'), `const fs = require('node:fs'); const calls = []; global.fetch = async (url, options) => { calls.push(JSON.parse(options.body)); fs.writeFileSync('calls.json', JSON.stringify(calls)); return {ok: true, json: async () => ({})}; };`);
    for (const [outcome, reporterFailed] of [['cancelled', ''], ['success', 'true']]) {
      fs.writeFileSync(path.join(dir, 'output'), '');
      const result = spawnSync(process.execPath, ['--require', path.join(dir, 'fake-api.cjs'), path.resolve(__dirname, 'bridge.cjs'), 'publish'], {cwd: dir, encoding: 'utf8', env: {...process.env, INVOCATION: JSON.stringify(invocation), JOBS: JSON.stringify(jobs), REPORT_OUTCOME: outcome, REPORT_FAILED: reporterFailed, GITHUB_EVENT_PATH: path.join(dir, 'event.json'), GITHUB_OUTPUT: path.join(dir, 'output'), GITHUB_STEP_SUMMARY: path.join(dir, 'summary'), GITHUB_REPOSITORY: invocation.repository, GITHUB_RUN_ID: invocation.run, GITHUB_RUN_ATTEMPT: invocation.attempt}});
      assert.equal(result.status, 0, result.stderr);
      assert.equal(fs.readFileSync(path.join(dir, 'output'), 'utf8'), 'failed=true\n');
      assert.match(fs.readFileSync(path.join(dir, 'summary'), 'utf8'), /Harness validation: \*\*failure\*\*/);
      assert.deepEqual(JSON.parse(fs.readFileSync(path.join(dir, 'calls.json'), 'utf8')).map(call => call.conclusion), ['failure', 'neutral']);
    }
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
test('report bridge refuses to let report generation rewrite failed test evidence', () => {
  const {digest} = require('./contract.cjs');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-report-'));
  try {
    const invocation = {version: 1, run: '123'};
    const result = {version: 1, invocation, status: 'failed', executed: 1};
    fs.mkdirSync(path.join(dir, 'bundle'));
    const resultPath = path.join(dir, 'bundle/result.json');
    const manifestPath = path.join(dir, 'bundle/manifest.json');
    fs.writeFileSync(resultPath, JSON.stringify(result));
    fs.writeFileSync(manifestPath, JSON.stringify({version: 1, invocation, files: [{path: 'result.json', sha256: digest(resultPath)}]}));
    const runner = path.join(dir, 'runner');
    fs.writeFileSync(runner, `#!/usr/bin/env node\nrequire('node:fs').writeFileSync('report.json', ${JSON.stringify(JSON.stringify({...result, status: 'passed'}))});`, {mode: 0o700});
    const report = spawnSync(process.execPath, [path.resolve(__dirname, 'bridge.cjs'), 'report'], {cwd: dir, encoding: 'utf8', env: {...process.env, INVOCATION: JSON.stringify(invocation), EXPECTED_MANIFEST_SHA: digest(manifestPath), RUNNER: runner}});
    assert.equal(report.status, 1);
    assert.match(report.stderr, /report changed execution outcome/);
  } finally { fs.rmSync(dir, {recursive: true, force: true}); }
});
