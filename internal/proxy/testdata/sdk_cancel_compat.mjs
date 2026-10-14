import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';

const { query } = await import(pathToFileURL(process.env.CLAUDE_PROXY_TEST_SDK_MODULE).href);
const abortController = new AbortController();
const deadline = setTimeout(() => abortController.abort(), 15_000);
const uuid = '33333333-3333-4333-8333-333333333333';
let closeInput;
const finish = new Promise(resolve => { closeInput = resolve; });
async function* prompts() {
  yield { type: 'user', uuid, message: { role: 'user', content: 'cancel before admission' }, parent_tool_use_id: null, session_id: '' };
  await finish;
}

const q = query({
  prompt: prompts(),
  options: {
    pathToClaudeCodeExecutable: process.env.CLAUDE_PROXY_TEST_BINARY,
    env: { ...process.env },
    abortController
  }
});

try {
  await q.initializationResult();
  // The Go test occupies the sole slot. Wait for the proxy to receive the
  // user frame so cancellation cannot race ahead of the async generator.
  const logPath = `${process.env.CODEX_PROXY_TEST_HOME}/.claude-proxy/claude-proxy.log`;
  let received = false;
  for (let i = 0; i < 200 && !received; i++) {
    const log = await readFile(logPath, 'utf8');
    received = log.split('\n').slice(0, -1).some(line => {
      const event = JSON.parse(line);
      return event.event === 'received' && event.type === 'user';
    });
    if (!received) await delay(10);
  }
  assert.ok(received, 'proxy did not receive SDK input');
  await q.applyFlagSettings({ permissions: { defaultMode: 'plan' } });
  const cancelled = await q.cancelAsyncMessage(uuid);
  assert.equal(cancelled, true);
  await q.getSettings();
  closeInput();
  let results = 0;
  for await (const message of q) if (message.type === 'result') results++;
  assert.equal(results, 0);
  console.log(JSON.stringify({ cancelled, results }));
} finally {
  clearTimeout(deadline);
  closeInput();
  q.close();
}
