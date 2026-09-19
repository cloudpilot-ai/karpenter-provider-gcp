const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const workflow = fs.readFileSync(path.join(__dirname, '../../.github/workflows/e2e.yaml'), 'utf8');
const job = name => workflow.split(`\n  ${name}:\n`)[1]?.split(/\n  [a-z-]+:\n/)[0];

test('workflow isolates code under test, runtime cloud access and publication', () => {
  assert.match(workflow, /\npermissions: \{\}/);
  for (const name of ['gate', 'tooling-preflight', 'build', 'runtime', 'reporter']) assert.ok(job(name), name);
  for (const name of ['tooling-preflight', 'build']) {
    assert.match(job(name), /permissions:\n      contents: read\n/);
    assert.doesNotMatch(job(name), /: write|environment:|concurrency:|google-github-actions/);
  }
  assert.match(job('runtime'), /environment: e2e-runtime/);
  assert.match(job('runtime'), /id-token: write/);
  assert.doesNotMatch(job('runtime'), /(?:checks|issues|pull-requests|contents): write|repository: \$\{\{ needs.gate.outputs.tested_repository/);
  assert.doesNotMatch(job('gate') + job('reporter'), /google-github-actions|repository: \$\{\{ needs.gate.outputs.tested_repository/);
  assert.match(job('build'), /if: needs.tooling-preflight.outputs.state == 'ready'/);
  assert.match(job('runtime'), /if: needs.tooling-preflight.outputs.state == 'ready'/);
  assert.ok(job('runtime').indexOf('bridge.cjs validate') < job('runtime').indexOf('google-github-actions/auth'));
  assert.doesNotMatch(workflow, /make e2e-(setup|teardown)|continue-on-error: true/);
  for (const match of workflow.matchAll(/uses: ([^\n]+)/g)) assert.match(match[1], /@[0-9a-f]{40} /);
  assert.equal((workflow.match(/retention-days: 30/g) || []).length, 6);
  assert.match(workflow, /artifact-ids: \$\{\{ needs.build.outputs.artifact_id \}\}/);
});
test('preflight and reporter retain diagnostic logs, including failures, before classification', () => {
  for (const name of ['tooling-preflight', 'reporter']) {
    assert.match(job(name), /name: Upload .*diagnostics/);
    assert.match(job(name), /if: always\(\) && hashFiles\('diagnostics\/\*\*'\) != ''/);
    assert.match(job(name), /retention-days: 30/);
    assert.match(job(name), /if-no-files-found: error/);
  }
  assert.ok(job('reporter').indexOf('name: Upload reporter diagnostics') < job('reporter').indexOf('name: Record reporter setup or data failure'));
  assert.match(job('reporter'), /name: Record reporter setup or data failure\n        if: failure\(\) \|\| cancelled\(\)/);
});
