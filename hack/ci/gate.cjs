const fs = require('node:fs');

function parseCommand({body, pullRequest, authorized}) {
  if (!pullRequest || authorized !== 'true') return null;
  return new Map([['/e2e', 'standard'], ['/e2e standard', 'standard'], ['/e2e gpu', 'gpu'], ['/e2e full', 'full']]).get(body) || null;
}

if (require.main === module) {
  const event = JSON.parse(fs.readFileSync(process.env.GITHUB_EVENT_PATH, 'utf8'));
  const mode = parseCommand({body: event.comment?.body, pullRequest: !!event.issue?.pull_request, authorized: process.env.COMMAND_AUTHORIZED});
  fs.appendFileSync(process.env.GITHUB_OUTPUT, `accepted=${mode !== null}\nmode=${mode || ''}\n`);
}
module.exports = {parseCommand};
