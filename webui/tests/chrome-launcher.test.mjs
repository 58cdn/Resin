import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { setTimeout as pause } from 'node:timers/promises';
import test from 'node:test';
import { waitForDevTools } from './chrome-launcher.cjs';

function fixture(t) {
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'resin-chrome-discovery-'));
  t.after(() => fs.rmSync(profile, { recursive: true, force: true }));
  const browser = Object.assign(new EventEmitter(), {
    stderr: new EventEmitter(), pid: 123, exitCode: null, signalCode: null,
  });
  return { browser, profile, portFile: path.join(profile, 'DevToolsActivePort') };
}

test('discovers DevTools from the profile file without any stderr output', async t => {
  const { browser, profile, portFile } = fixture(t);
  fs.writeFileSync(portFile, '54321\r\n/devtools/browser/test-browser\r\n');
  assert.equal(await waitForDevTools(browser, profile), 'ws://127.0.0.1:54321/devtools/browser/test-browser');
  assert.equal(browser.listenerCount('error'), 0);
  assert.equal(browser.stderr.listenerCount('data'), 0);
});

test('waits for a partially written port file to contain the browser target', async t => {
  const { browser, profile, portFile } = fixture(t);
  fs.writeFileSync(portFile, '54321\n');
  const ready = waitForDevTools(browser, profile, { timeoutMs: 1000 });
  await pause(20);
  fs.writeFileSync(portFile, '54321\n/devtools/browser/complete');
  assert.equal(await ready, 'ws://127.0.0.1:54321/devtools/browser/complete');
});

test('retains stderr discovery when the profile file is absent', async t => {
  const { browser, profile } = fixture(t);
  const ready = waitForDevTools(browser, profile, { timeoutMs: 1000 });
  browser.stderr.emit('data', Buffer.from('DevTools listening on '));
  browser.stderr.emit('data', Buffer.from('ws://127.0.0.1:54321/devtools/browser/fallback\n'));
  assert.equal(await ready, 'ws://127.0.0.1:54321/devtools/browser/fallback');
});

test('reports a spawn error with its original cause', async t => {
  const { browser, profile } = fixture(t);
  const ready = waitForDevTools(browser, profile, { timeoutMs: 1000 });
  const error = new Error('spawn chrome ENOENT');
  browser.emit('error', error);
  await assert.rejects(ready, failure => failure.cause === error && /failed to spawn/.test(failure.message));
});

test('reports an early process exit and stderr rather than waiting for timeout', async t => {
  const { browser, profile } = fixture(t);
  const ready = waitForDevTools(browser, profile, { timeoutMs: 1000 });
  browser.stderr.emit('data', Buffer.from('profile is locked'));
  browser.exitCode = 7;
  await assert.rejects(ready, /Chrome exited.*code=7.*profile is locked/);
});

test('reports missing startup evidence and empty stderr on timeout', async t => {
  const { browser, profile } = fixture(t);
  await assert.rejects(waitForDevTools(browser, profile, { timeoutMs: 20 }),
    /within 20ms.*DevToolsActivePort=not created.*stderr: \(empty\)/);
});

test('rejects invalid ports and non-browser targets instead of connecting', async t => {
  const { browser, profile, portFile } = fixture(t);
  for (const content of ['0\n/devtools/browser/test', '65536\n/devtools/browser/test',
    '12x\n/devtools/browser/test', '54321\n//remote.example/test', '54321\n/devtools/page/test']) {
    fs.writeFileSync(portFile, content);
    await assert.rejects(waitForDevTools(browser, profile, { timeoutMs: 1 }), /incomplete or invalid/);
  }
});
