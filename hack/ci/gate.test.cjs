const {test} = require('node:test');
const assert = require('node:assert/strict');
const {parseCommand} = require('./gate.cjs');

test('only authorized PR comments with exact commands are accepted', () => {
  for (const [body, mode] of [['/e2e', 'standard'], ['/e2e standard', 'standard'], ['/e2e gpu', 'gpu'], ['/e2e full', 'full']]) {
    assert.equal(parseCommand({body, pullRequest: true, authorized: 'true'}), mode);
  }
  for (const body of ['/e2e ', '/e2e\n', '/e2e standard extra', '/e2e unknown', ' /e2e', '/e2e\tgpu', '/e2e gpu\n/run']) {
    assert.equal(parseCommand({body, pullRequest: true, authorized: 'true'}), null);
  }
  assert.equal(parseCommand({body: '/e2e', pullRequest: false, authorized: 'true'}), null);
  for (const authorized of ['false', '', undefined, true]) {
    assert.equal(parseCommand({body: '/e2e', pullRequest: true, authorized}), null);
  }
});
