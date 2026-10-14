// Mock-only browser integration test. Requires Node 22+ and Chrome/Chromium.
// Uses a new browser profile and loopback port; never contacts a Resin instance.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const { spawn } = require('node:child_process');
const assert = require('node:assert/strict');
const esbuild = require('esbuild');
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const results = [];

async function main() {
  const chrome = [process.env.CHROME_PATH, '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/usr/bin/chromium', '/usr/bin/chromium-browser',
    ...['PROGRAMFILES', 'PROGRAMFILES(X86)', 'LOCALAPPDATA'].map(key => process.env[key] && path.join(process.env[key], 'Google/Chrome/Application/chrome.exe'))].find(file => file && fs.existsSync(file));
  assert.ok(chrome, 'Chrome/Chromium is required; set CHROME_PATH to its executable');
  const output = fs.mkdtempSync(path.join(os.tmpdir(), 'resin-cache-browser-'));
  const artifacts = process.env.BROWSER_ARTIFACTS_DIR ? path.resolve(process.env.BROWSER_ARTIFACTS_DIR) : output;
  fs.mkdirSync(artifacts, { recursive: true });
  await esbuild.build({ entryPoints: [path.join(__dirname, 'ip-quality-list.fixture.tsx')], outdir: output, entryNames: 'app', bundle: true, loader: { '.woff2': 'file', '.woff': 'file' }, define: { 'import.meta.env.VITE_API_BASE_URL': '""', 'process.env.NODE_ENV': '"production"' }, jsx: 'automatic' });
  fs.writeFileSync(path.join(output, 'index.html'), '<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/app.css"><title>Resin cache fixture</title><style>body{padding:16px}#root{max-width:1280px;margin:auto}</style><div id="root"></div><script src="/app.js"></script></html>');
  const server = http.createServer((req, res) => {
    const file = path.join(output, path.basename(req.url.split('?')[0]) || 'index.html');
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) { res.writeHead(404); res.end(); return; }
    res.setHeader('Content-Type', { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.woff2': 'font/woff2', '.woff': 'font/woff' }[path.extname(file)] || 'application/octet-stream');
    res.end(fs.readFileSync(file));
  });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  const browser = spawn(chrome, ['--headless=new', '--disable-gpu', '--disable-background-networking', '--no-first-run', '--no-default-browser-check', '--remote-debugging-port=0', `--user-data-dir=${path.join(output, 'profile')}`, 'about:blank'], { windowsHide: true, stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '', spawnError;
  browser.on('error', error => { spawnError = error; });
  browser.stderr.on('data', chunk => { stderr += chunk; });
  let socket, command;
  try {
    for (let i = 0; i < 150 && !/DevTools listening on (ws:\/\/[^\s]+)/.test(stderr) && !spawnError; i++) await pause(100);
    if (spawnError) throw spawnError;
    const endpoint = stderr.match(/DevTools listening on (ws:\/\/[^\s]+)/)?.[1];
    assert.ok(endpoint, 'Chrome DevTools did not start: ' + stderr.slice(-1000));
    socket = new WebSocket(endpoint);
    await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = reject; });
    let sequence = 0;
    const pending = new Map(), exceptions = [];
    socket.onmessage = event => {
      const data = JSON.parse(event.data);
      if (data.id) { const request = pending.get(data.id); if (!request) return; pending.delete(data.id); clearTimeout(request.timer); data.error ? request.reject(new Error(JSON.stringify(data.error))) : request.resolve(data.result); }
      else if (data.method === 'Runtime.exceptionThrown') exceptions.push(data.params);
    };
    command = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
      const id = ++sequence;
      const timer = setTimeout(() => { pending.delete(id); reject(new Error('CDP timeout: ' + method)); }, 15000);
      pending.set(id, { resolve, reject, timer });
      socket.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
    const { targetId } = await command('Target.createTarget', { url: 'about:blank' });
    const { sessionId } = await command('Target.attachToTarget', { targetId, flatten: true });
    const call = (method, params) => command(method, params, sessionId);
    const evaluate = async expression => {
      const response = await call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
      if (response.exceptionDetails) throw new Error(JSON.stringify(response.exceptionDetails));
      return response.result.value;
    };
    const waitFor = async (expression, timeout = 5000) => {
      const start = Date.now();
      while (Date.now() - start < timeout) { if (await evaluate(expression)) return; await pause(50); }
      throw new Error('Timed out: ' + expression + '\n' + await evaluate('JSON.stringify({toolbar:document.querySelector(".ip-quality-list-toolbar")?.innerText,requests:fixture.requests.map(r=>({...r,hashes:r.hashes.length})),active:document.activeElement?.outerHTML.slice(0,300)})'));
    };
    const includes = text => 'document.body.innerText.includes(' + JSON.stringify(text) + ')';
    const batchRequests = 'fixture.requests.filter(r=>r.method==="POST")';
    const button = text => 'Array.from(document.querySelectorAll("button")).find(b=>b.textContent===' + JSON.stringify(text) + ')';
    const read = button("Read this page's quality cache");
    const cancel = button('Cancel reading');
    const reset = async scenario => { await evaluate(`fixture.scenario(${JSON.stringify(scenario)})`); await waitFor('document.querySelectorAll(".ip-quality-cache-button").length===70'); };
    const clickRead = async () => { await evaluate(`${read}.click()`); };
    const key = async (name, code, modifiers = 0) => { await call('Input.dispatchKeyEvent', { type: 'keyDown', key: name, code, ...(name === 'Enter' ? { text: '\r' } : {}), windowsVirtualKeyCode: name === 'Tab' ? 9 : name === 'Escape' ? 27 : 13, modifiers }); await call('Input.dispatchKeyEvent', { type: 'keyUp', key: name, code, windowsVirtualKeyCode: name === 'Tab' ? 9 : name === 'Escape' ? 27 : 13, modifiers }); };
    await call('Runtime.enable', {}); await call('Page.enable', {});
    await call('Emulation.setDeviceMetricsOverride', { width: 1280, height: 1050, deviceScaleFactor: 1, mobile: false });
    await call('Page.navigate', { url: `http://127.0.0.1:${server.address().port}/` });
    await waitFor('document.querySelectorAll(".ip-quality-cache-button").length===70');
    assert.equal(await evaluate(`${batchRequests}.length`), 0);
    await evaluate(`${read}.focus()`); await key('Enter', 'Enter');
    await waitFor(includes('Read cache for 70 / 70 nodes'));
    assert.deepEqual(await evaluate(`${batchRequests}.map(r=>r.hashes.length)`), [50, 20]);
    assert.ok(await evaluate(`${batchRequests}[1].at-${batchRequests}[0].at>=550`));
    assert.equal(await evaluate('fixture.requests.some(r=>r.path.endsWith("/ip-check")||r.path.endsWith("/sources"))'), false);
    for (const label of ['Cached', 'Partial source failure', 'No cache', 'Egress unavailable or changed', 'Large result; open details', 'Egress changed', 'Sources unsuccessful', 'Read failed', 'Unknown']) assert.equal(await evaluate(includes(label)), true, label);
    results.push('explicit keyboard read only; 70 nodes use 50+20 spaced requests; no detail N+1; mixed item/source outcomes stay distinct');
    const screenshot = async name => { const shot = await call('Page.captureScreenshot', { format: 'png' }); fs.writeFileSync(path.join(artifacts, name + '.png'), Buffer.from(shot.data, 'base64')); };
    await screenshot('list-desktop');

    await evaluate('document.querySelector(".ip-quality-cache-button").focus()'); await key('Enter', 'Enter');
    await waitFor('document.querySelector("[role=dialog]")?.contains(document.activeElement)');
    await waitFor(includes('Fixture city'));
    assert.equal(await evaluate('fixture.requests.filter(r=>r.path.endsWith("/ip-check")).length'), 1);
    assert.equal(await evaluate('fixture.requests.filter(r=>r.path.endsWith("/sources")).length'), 1);
    await key('Tab', 'Tab', 8);
    assert.equal(await evaluate('document.querySelector("[role=dialog]").contains(document.activeElement)'), true);
    await key('Tab', 'Tab');
    assert.equal(await evaluate('document.activeElement.getAttribute("aria-label")'), 'Close detail panel');
    const accessibility = await call('Accessibility.getFullAXTree', {});
    assert.ok(accessibility.nodes.some(node => node.role?.value === 'dialog' && node.name?.value.includes('Fixture node')));
    await key('Escape', 'Escape');
    await waitFor('document.activeElement.classList.contains("ip-quality-cache-button")');
    results.push('badge opens PR14 cache detail using two GETs; named dialog, focus entry, Tab wrap, Escape and focus restoration');

    await reset('cancel-second'); await clickRead();
    await waitFor(`${batchRequests}.length===2`);
    assert.equal(await evaluate('document.querySelector("progress").getAttribute("aria-label")'), 'Page cache reading progress');
    await evaluate(`${cancel}.focus()`); await key('Enter', 'Enter');
    await waitFor(includes('Canceled; completed cache reads are retained.'));
    assert.equal(await evaluate(`${batchRequests}[1].aborted`), true);
    assert.equal(await evaluate(includes('Read cache for 50 / 70 nodes')), true);
    assert.equal(await evaluate(`document.activeElement===${read}`), true);
    results.push('keyboard cancellation aborts active second batch, retains completed items and restores read focus');

    await reset('mixed'); await clickRead(); await waitFor(includes('Read cache for 50 / 70 nodes'));
    await evaluate(`${cancel}.click()`); await pause(700);
    assert.equal(await evaluate(`${batchRequests}.length`), 1);
    results.push('cancel during spacing prevents the next batch');

    for (const [scenario, message] of [['limited', 'Too many reads'], ['disabled', 'plugin is disabled'], ['unauthorized', 'Access denied'], ['server-error', 'Page cache reading stopped']]) {
      await reset(scenario); await clickRead(); await waitFor(includes(message)); await pause(650);
      assert.equal(await evaluate(`${batchRequests}.length`), 1);
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-cache-button small").length'), 0);
      results.push(scenario + ': explicit error, no automatic retry, no reassuring facts');
    }
    await reset('disabled-second'); await clickRead(); await waitFor(includes('plugin is disabled'));
    assert.equal(await evaluate('document.querySelectorAll(".ip-quality-cache-button small").length'), 0);
    assert.equal(await evaluate(includes('Read cache for 0 / 70 nodes')), true);
    results.push('plugin unavailable in second batch clears earlier batch facts');

    await reset('expiry'); await clickRead(); await waitFor(includes('Read cache for 70 / 70 nodes'));
    await waitFor('document.querySelectorAll(".ip-quality-cache-button small").length===0');
    assert.equal(await evaluate(`${batchRequests}.length`), 2);
    assert.equal(await evaluate(includes('Cache expired')), true);
    results.push('expiry removes badge facts without a refresh or probe');

    for (const change of ['page', 'filter', 'version', 'unmount']) {
      await reset('pending'); await clickRead(); await waitFor(`${batchRequests}.length===1`);
      if (change === 'page') await evaluate(`${button('Next')}.click()`);
      if (change === 'filter') await evaluate('(()=>{const input=document.querySelector(".nodes-inline-filters input");Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,"value").set.call(input,"changed");input.dispatchEvent(new Event("input",{bubbles:true}));})()');
      if (change === 'version') await evaluate('fixture.changeVersion()');
      if (change === 'unmount') await evaluate('fixture.unmount()');
      await waitFor(`${batchRequests}[0].aborted`);
      await pause(650);
      assert.equal(await evaluate(`${batchRequests}.length`), 1);
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-cache-button small").length'), 0);
      results.push(change + ' cancels old scope without automatic cache read');
    }
    for (const scenario of ['pending', 'mixed']) {
      await reset(scenario); await clickRead();
      await waitFor(scenario === 'pending' ? `${batchRequests}.length===1` : includes('Read cache for 70 / 70 nodes'));
      const before = await evaluate(`${batchRequests}.length`);
      const edit = value => evaluate('(()=>{const input=document.querySelector(".nodes-inline-filters input");Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,"value").set.call(input,' + JSON.stringify(value) + ');input.dispatchEvent(new Event("input",{bubbles:true}));})()');
      await edit('temporary');
      await waitFor(includes("This page's quality cache has not been read."));
      await edit('');
      await waitFor(`!${read}.disabled`);
      assert.equal(await evaluate(includes("This page's quality cache has not been read.")), true);
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-cache-button small").length'), 0);
      await evaluate('fixture.setScenario("mixed")'); await clickRead();
      await waitFor(includes('Read cache for 70 / 70 nodes'));
      assert.equal(await evaluate(`${batchRequests}.length`), before + 2);
      results.push(scenario + ' A-to-B-to-A scope round trip does not revive stale state and permits a new manual read');
    }
    await reset('timeout'); await clickRead();
    await waitFor(includes('Cache read timed out'), 23000);
    assert.equal(await evaluate(`${batchRequests}[0].aborted`), true);
    assert.equal(await evaluate(`${batchRequests}.length`), 1);
    results.push('real 20 second request timeout aborts transport and stops later batches');

    await reset('mixed'); await clickRead(); await waitFor(includes('Read cache for 70 / 70 nodes'));
    await evaluate('fixture.locale("zh-CN")'); await waitFor(includes('读取本页质量缓存'));
    assert.equal(await evaluate(includes('部分来源失败')), true);
    await call('Emulation.setDeviceMetricsOverride', { width: 390, height: 1000, deviceScaleFactor: 1, mobile: true });
    await pause(150);
    assert.equal(await evaluate('document.documentElement.scrollWidth<=window.innerWidth'), true);
    await screenshot('list-mobile');
    assert.equal(await evaluate('fixture.forbidden.length'), 0);
    assert.equal(exceptions.length, 0, JSON.stringify(exceptions));
    results.push('Chinese labels, 390px viewport without document overflow, no forbidden fetch or uncaught exception');
    fs.writeFileSync(path.join(artifacts, 'browser-results.json'), JSON.stringify({ passed: true, checks: results }, null, 2));
    console.log(JSON.stringify({ passed: true, checks: results, artifacts }, null, 2));
  } finally {
    if (command) await command('Browser.close').catch(() => {});
    if (socket) socket.close();
    server.close();
    browser.kill();
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
