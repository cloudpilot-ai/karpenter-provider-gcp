const {requireValue, validateResult} = require('./contract.cjs');

async function publish({invocation, target, jobs, result, evidenceError = false, render = () => {}}, api) {
  requireValue(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(target.repository) && Number.isSafeInteger(target.pr) && target.pr > 0 && /^\d+$/.test(target.run) && /^\d+$/.test(target.attempt), 'invalid publication target');
  if (invocation) requireValue(invocation.repository === target.repository && invocation.pr === target.pr && invocation.run === target.run && invocation.attempt === target.attempt, 'publication identity mismatch');
  if (result) validateResult(result, invocation);
  const failed = !invocation || (jobs['tooling-preflight']?.outputs?.state === 'ready' && !result) || evidenceError || result?.status === 'failed' || Object.values(jobs).some(job => ['failure', 'cancelled'].includes(job.result));
  const state = failed ? 'failure' : jobs['tooling-preflight']?.outputs?.state || 'not_implemented';
  const runURL = `https://github.com/${target.repository}/actions/runs/${target.run}/attempts/${target.attempt}`;
  const tested = invocation?.tested?.sha;
  const tooling = invocation?.tooling?.sha;
  const real = failed ? (jobs.runtime?.result !== 'skipped' ? 'failure' : 'neutral') : result?.status === 'passed' ? 'success' : 'neutral';
  const markdown = `### E2E Test Results\n\nHarness validation: **${failed ? 'failure' : 'success'}**. Real e2e: **${real === 'success' ? 'passed' : real === 'failure' ? 'failed or incomplete' : 'not run'}** (${state}).\n\nTested SHA: ${tested ? '`' + tested + '`' : 'unavailable (capture failed)'}.\nTooling SHA: ${tooling ? '`' + tooling + '`' : 'not resolved'}.\n\n[Workflow run](${runURL}) · [Artifacts](${runURL}#artifacts)\n`;
  render(markdown);
  try {
    if (tested) {
      requireValue(/^[0-9a-f]{40}$/.test(tested), 'invalid tested SHA');
      for (const [name, conclusion] of [['E2E harness validation', failed ? 'failure' : 'success'], [`E2E ${invocation?.mode || 'standard'}`, real]]) {
        await api(`/repos/${target.repository}/check-runs`, {method: 'POST', body: JSON.stringify({name, head_sha: tested, status: 'completed', conclusion, details_url: runURL, external_id: `${target.run}-${target.attempt}`, output: {title: name, summary: markdown}})});
      }
    }
  } catch (error) {
    render(markdown.replace('Harness validation: **success**', 'Harness validation: **failure**') + '\nCheck publication failed; see workflow logs.\n');
    throw error;
  }
  return {markdown, failed};
}
module.exports = {publish};
