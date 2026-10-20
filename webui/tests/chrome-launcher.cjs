const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { setTimeout: pause } = require('node:timers/promises');

// Chrome writes this file for --remote-debugging-port=0 even when its
// "DevTools listening" message is not forwarded through stderr on Windows.
async function waitForDevTools(browser, profile, { timeoutMs = 15000 } = {}) {
  const portFile = path.join(profile, 'DevToolsActivePort');
  let stderr = '', spawnError;
  const onError = error => { spawnError = error; };
  const onData = chunk => { stderr = (stderr + chunk).slice(-8192); };
  browser.on('error', onError);
  browser.stderr.on('data', onData);
  const deadline = Date.now() + timeoutMs;
  let fileState = 'not created';
  try {
    while (true) {
      if (spawnError) throw new Error(`Chrome failed to spawn: ${spawnError.message}`, { cause: spawnError });
      if (browser.exitCode !== null || browser.signalCode !== null) {
        throw new Error(`Chrome exited before DevTools was ready (code=${browser.exitCode}, signal=${browser.signalCode}). stderr: ${stderr || '(empty)'}`);
      }
      try {
        const [port, target] = fs.readFileSync(portFile, 'utf8').trim().split(/\r?\n/);
        // A partially written file is normal during startup. Never connect to
        // an arbitrary host or an incomplete target from malformed contents.
        if (/^\d+$/.test(port) && Number(port) > 0 && Number(port) <= 65535 &&
            /^\/devtools\/browser\/[a-zA-Z0-9-]+$/.test(target)) {
          return `ws://127.0.0.1:${port}${target}`;
        }
        fileState = 'incomplete or invalid';
      } catch (error) {
        if (error.code !== 'ENOENT') throw error;
      }
      const endpoint = stderr.match(/DevTools listening on (ws:\/\/[^\s]+)/)?.[1];
      if (endpoint) return endpoint;
      if (Date.now() >= deadline) {
        throw new Error(`Chrome DevTools did not start within ${timeoutMs}ms (pid=${browser.pid}, DevToolsActivePort=${fileState}). stderr: ${stderr || '(empty)'}`);
      }
      await pause(Math.min(100, Math.max(1, deadline - Date.now())));
    }
  } finally {
    browser.removeListener('error', onError);
    browser.stderr.removeListener('data', onData);
  }
}

function launchChrome(chrome, profile) {
  const browser = spawn(chrome, [
    '--headless=new', '--disable-gpu', '--disable-background-networking',
    '--no-first-run', '--no-default-browser-check', '--remote-debugging-port=0',
    `--user-data-dir=${profile}`, 'about:blank',
  ], { windowsHide: true, stdio: ['ignore', 'ignore', 'pipe'] });
  // Keep draining stderr after discovery so browser diagnostics cannot block
  // the child. Every caller owns this isolated process and closes it in finally.
  browser.stderr.resume();
  return { browser, ready: waitForDevTools(browser, profile) };
}

module.exports = { launchChrome, waitForDevTools };
