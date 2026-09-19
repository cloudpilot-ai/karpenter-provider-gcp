const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const {isDeepStrictEqual} = require('node:util');
const TOOLING_REPOSITORY = 'cloudpilot-ai/karpenter-provider-gcp';
const SHA = /^[0-9a-f]{40}$/;
const REPOSITORY = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
function requireValue(ok, message) { if (!ok) throw Error(message); }

async function capture(config, api) {
  requireValue(REPOSITORY.test(config.repository) && Number.isSafeInteger(config.pr) && config.pr > 0, 'invalid reporting target');
  requireValue(['standard', 'gpu', 'full'].includes(config.mode), 'invalid mode');
  requireValue(/^\d+$/.test(config.run) && /^\d+$/.test(config.attempt), 'invalid invocation');
  let tooling = null;
  if (config.mode === 'standard') {
    requireValue(/^[A-Za-z0-9][A-Za-z0-9._/-]*$/.test(config.toolingRef) && !config.toolingRef.includes('..') && !config.toolingRef.endsWith('/') && !config.toolingRef.endsWith('.lock'), 'invalid tooling branch');
    const ref = await api(`/repos/${TOOLING_REPOSITORY}/git/ref/heads/${config.toolingRef}`);
    requireValue(ref.ref === `refs/heads/${config.toolingRef}` && ref.object?.type === 'commit' && SHA.test(ref.object.sha), 'invalid tooling ref response');
    tooling = {repository: TOOLING_REPOSITORY, ref: config.toolingRef, sha: ref.object.sha};
  }
  const pr = await api(`/repos/${config.repository}/pulls/${config.pr}`);
  requireValue(SHA.test(pr.head?.sha) && REPOSITORY.test(pr.head?.repo?.full_name), 'invalid PR source');
  return {version: 1, repository: config.repository, pr: config.pr, run: config.run, attempt: config.attempt, mode: config.mode,
    tooling,
    tested: {repository: pr.head.repo.full_name, sha: pr.head.sha}, enabled: tooling !== null && config.enabled === true, target: tooling ? {...config.target} : {}};
}
module.exports = {capture, requireValue};

function capability(value, invocation) {
  requireValue(value?.version === 1 && Array.isArray(value.commands) && ['capabilities', 'prepare', 'run', 'report'].every(c => value.commands.includes(c)), 'incompatible capability contract');
  requireValue(['supported', 'not_implemented'].includes(value.modes?.standard), 'missing or malformed standard capability');
  if (invocation.mode !== 'standard' || value.modes.standard === 'not_implemented') return 'not_implemented';
  return invocation.enabled ? 'ready' : 'disabled';
}
module.exports.capability = capability;

function readJSON(file) {
  const stat = fs.lstatSync(file);
  requireValue(stat.isFile() && stat.size <= 1024 * 1024, 'invalid JSON file or size');
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}
function digest(file) {
  const hash = crypto.createHash('sha256');
  const fd = fs.openSync(file, 'r');
  try {
    const buffer = Buffer.alloc(1024 * 1024);
    let count;
    while ((count = fs.readSync(fd, buffer)) > 0) hash.update(buffer.subarray(0, count));
    return hash.digest('hex');
  } finally { fs.closeSync(fd); }
}
function validateBundle(directory, invocation) {
  requireValue(fs.lstatSync(directory).isDirectory(), 'invalid bundle directory');
  const manifest = readJSON(path.join(directory, 'manifest.json'));
  requireValue(manifest.version === 1 && isDeepStrictEqual(manifest.invocation, invocation), 'bundle identity mismatch');
  requireValue(Array.isArray(manifest.files) && manifest.files.length > 0 && manifest.files.length <= 10000, 'invalid bundle inventory');
  const names = new Set();
  let size = 0;
  for (const file of manifest.files) {
    requireValue(typeof file.path === 'string' && /^[A-Za-z0-9_./-]+$/.test(file.path) && !file.path.startsWith('/') && file.path !== 'manifest.json' && file.path.split('/').every(p => p && p !== '.' && p !== '..') && !names.has(file.path), 'unsafe or duplicate bundle path');
    names.add(file.path);
    let current = directory;
    for (const segment of file.path.split('/')) {
      current = path.join(current, segment);
      requireValue(!fs.lstatSync(current).isSymbolicLink(), 'bundle symlink');
    }
    const stat = fs.lstatSync(current);
    size += stat.size;
    requireValue(stat.isFile() && size <= 4 * 1024 ** 3, 'invalid bundle file or size');
    requireValue(/^[0-9a-f]{64}$/.test(file.sha256) && digest(current) === file.sha256, 'bundle digest mismatch');
  }
  const walk = (dir, prefix = '') => {
    for (const entry of fs.readdirSync(dir, {withFileTypes: true})) {
      const name = prefix + entry.name;
      if (entry.isDirectory()) walk(path.join(dir, entry.name), name + '/');
      else requireValue(entry.isFile() && (name === 'manifest.json' || names.has(name)), 'unlisted or unsafe bundle entry');
    }
  };
  walk(directory);
  return manifest;
}
module.exports.readJSON = readJSON;
module.exports.validateBundle = validateBundle;
module.exports.digest = digest;

function validateResult(result, invocation) {
  requireValue(result?.version === 1 && isDeepStrictEqual(result.invocation, invocation), 'result identity mismatch');
  requireValue(['passed', 'failed', 'not_implemented'].includes(result.status), 'invalid result status');
  requireValue(Number.isSafeInteger(result.executed) && result.executed >= 0 && (result.status !== 'passed' || result.executed > 0) && (result.status !== 'not_implemented' || result.executed === 0), 'invalid execution count');
  return result;
}
module.exports.validateResult = validateResult;
