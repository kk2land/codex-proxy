import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';

const { query } = await import(pathToFileURL(process.env.CLAUDE_PROXY_TEST_SDK_MODULE).href);
const abortController = new AbortController();
const deadline = setTimeout(() => abortController.abort(), 15_000);
let nextInput;
let closeInput;
const next = new Promise(resolve => { nextInput = resolve; });
const finish = new Promise(resolve => { closeInput = resolve; });

async function* prompts() {
  yield { type: 'user', message: { role: 'user', content: 'fixture first input' }, parent_tool_use_id: null, session_id: '' };
  await next;
  yield { type: 'user', message: { role: 'user', content: 'fixture second input' }, parent_tool_use_id: null, session_id: '' };
  await finish;
}

let approvals = 0;
const q = query({
  prompt: prompts(),
  options: {
    pathToClaudeCodeExecutable: process.env.CLAUDE_PROXY_TEST_BINARY,
    env: { ...process.env },
    abortController,
    resume: '11111111-1111-4111-8111-111111111111',
    permissionMode: 'plan',
    systemPrompt: { type: 'preset', preset: 'claude_code' },
    tools: { type: 'preset', preset: 'claude_code' },
    settingSources: ['user', 'project', 'local'],
    includePartialMessages: true,
    canUseTool: async (_tool, input) => {
      approvals++;
      return { behavior: 'allow', updatedInput: input };
    }
  }
});

try {
  let results = 0;
  for await (const message of q) {
    if (message.type !== 'result') continue;
    assert.equal(message.result, 'fixture-ok');
    results++;
    if (results === 1) {
      await q.setPermissionMode('plan');
      await q.setModel('sonnet');
      nextInput();
    } else {
      await q.interrupt();
      closeInput();
    }
  }
  assert.equal(results, 2);
  assert.equal(approvals, 2);
  console.log(JSON.stringify({ results, approvals }));
} finally {
  clearTimeout(deadline);
  nextInput();
  closeInput();
  q.close();
}
