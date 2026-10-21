// Mock-only browser integration test. Requires Node 22+ and Chrome/Chromium.
// Uses a new browser profile and loopback port; never contacts a Resin instance.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const http = require('node:http');
const { launchChrome } = require('./chrome-launcher.cjs');
const assert = require('node:assert/strict');
const esbuild = require('esbuild');
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const results = [];

async function main() {
  const chrome = [process.env.CHROME_PATH, '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/usr/bin/chromium', '/usr/bin/chromium-browser',
    ...['PROGRAMFILES', 'PROGRAMFILES(X86)', 'LOCALAPPDATA'].map(key => process.env[key] && path.join(process.env[key], 'Google/Chrome/Application/chrome.exe'))].find(file => file && fs.existsSync(file));
  assert.ok(chrome, 'Chrome/Chromium is required; set CHROME_PATH to its executable');
  const output = fs.mkdtempSync(path.join(os.tmpdir(), 'resin-byip-browser-'));
  const artifacts = process.env.BROWSER_ARTIFACTS_DIR ? path.resolve(process.env.BROWSER_ARTIFACTS_DIR) : output;
  fs.mkdirSync(artifacts, { recursive: true });
  await esbuild.build({ entryPoints: [path.join(__dirname, 'ip-quality-byip.fixture.tsx')], outdir: output, entryNames: 'app', bundle: true, loader: { '.woff2': 'file', '.woff': 'file' }, define: { 'import.meta.env.VITE_API_BASE_URL': '""', 'process.env.NODE_ENV': '"production"' }, jsx: 'automatic' });
  fs.writeFileSync(path.join(output, 'index.html'), '<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/app.css"><title>Resin by-IP cache fixture</title><style>body{padding:16px}#root{max-width:1280px;margin:auto}</style><div id="root"></div><script src="/app.js"></script></html>');
  const server = http.createServer((req, res) => {
    const file = path.join(output, path.basename(req.url.split('?')[0]) || 'index.html');
    if (!fs.existsSync(file) || !fs.statSync(file).isFile()) { res.writeHead(404); res.end(); return; }
    res.setHeader('Content-Type', { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.woff2': 'font/woff2', '.woff': 'font/woff' }[path.extname(file)] || 'application/octet-stream');
    res.end(fs.readFileSync(file));
  });
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  const { browser, ready } = launchChrome(chrome, path.join(output, 'profile'));
  let socket, command;
  try {
    const endpoint = await ready;
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
      while (Date.now() - start < timeout) { if (await evaluate(`Boolean(${expression})`)) return; await pause(50); }
      throw new Error('Timed out: ' + expression + '\n' + await evaluate('JSON.stringify({status:document.querySelector(".ip-cache-status")?.innerText,requests:fixture.requests,active:document.activeElement?.outerHTML.slice(0,300)})'));
    };
    const includes = text => 'document.body.innerText.includes(' + JSON.stringify(text) + ')';
    const button = text => 'Array.from(document.querySelectorAll("button")).find(b=>b.textContent===' + JSON.stringify(text) + ')';
    const read = button('Read cache');
    const cancel = button('Cancel reading');
    const queryRequests = 'fixture.requests.filter(r=>r.path.startsWith("/api/v1/ip-checks/"))';
    const key = async (name, code, modifiers = 0) => {
      const keyCode = name === 'Tab' ? 9 : name === 'Escape' ? 27 : 13;
      await call('Input.dispatchKeyEvent', { type: 'keyDown', key: name, code, windowsVirtualKeyCode: keyCode, modifiers, ...(name === 'Enter' ? { text: '\r' } : {}) });
      await call('Input.dispatchKeyEvent', { type: 'keyUp', key: name, code, windowsVirtualKeyCode: keyCode, modifiers });
    };
    const input = value => evaluate('(()=>{const input=document.querySelector("#ip-cache-address");Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,"value").set.call(input,' + JSON.stringify(value) + ');input.dispatchEvent(new Event("input",{bubbles:true}));})()');
    const reset = async (scenario, entry = '/resources/ip-quality') => {
      await evaluate(`fixture.scenario(${JSON.stringify(scenario)},${JSON.stringify(entry)})`);
      await waitFor(entry === '/resources' ? `document.querySelector('a[href="/resources/ip-quality"]')` : 'document.querySelector("#ip-cache-address")');
    };
    const submit = async (ip = '8.8.8.8') => { await input(ip); await evaluate(`${read}.click()`); };
    const screenshot = async name => { const shot = await call('Page.captureScreenshot', { format: 'png' }); fs.writeFileSync(path.join(artifacts, name + '.png'), Buffer.from(shot.data, 'base64')); };
    await call('Runtime.enable', {}); await call('Page.enable', {});
    await call('Emulation.setDeviceMetricsOverride', { width: 1280, height: 1050, deviceScaleFactor: 1, mobile: false });
    await call('Page.navigate', { url: `http://127.0.0.1:${server.address().port}/` });
    await waitFor('document.querySelector("#ip-cache-address")');
    await input('8.8.8.8');
    await evaluate('window.dispatchEvent(new Event("focus"));window.dispatchEvent(new Event("online"))');
    await pause(100);
    assert.equal(await evaluate('fixture.requests.length'), 0);
    assert.equal(await evaluate(includes('Enter a public IP')), true);
    results.push('initial, edit, window focus and reconnect are idle with explicit empty-state guidance');

    for (const value of ['', 'sources', 'lookup', 'example.com', 'https://8.8.8.8', '8.8.8.8:443', '8.8.8.8/32', 'fe80::1%eth0', '999.8.8.8']) {
      await input(value); await evaluate(`${read}.click()`);
      await waitFor('document.querySelector("#ip-cache-address").getAttribute("aria-invalid")==="true"');
      assert.equal(await evaluate('document.activeElement.id'), 'ip-cache-address');
      assert.equal(await evaluate('fixture.requests.length'), 0);
    }
    results.push('invalid and route-colliding inputs never fetch; inline alert, described error and input focus');

    for (const [address, canonical] of [['8.8.8.8', '8.8.8.8'], ['2001:4860:4860:0:0:0:0:8888', '2001:4860:4860::8888'], ['::ffff:8.8.8.8', '8.8.8.8']]) {
      await reset('success'); await input(address); await evaluate('document.querySelector("#ip-cache-address").focus()'); await key('Enter', 'Enter');
      await waitFor(includes('Valid cache read'));
      assert.deepEqual(await evaluate('fixture.requests.map(r=>r.path)'), [`/api/v1/ip-checks/${canonical}`, '/api/v1/ip-checks/sources']);
      assert.equal(await evaluate('fixture.requests.every(r=>r.method==="GET"&&r.cache==="no-store")'), true);
      assert.equal(await evaluate(includes('0 / Unknown')), true);
      assert.equal(await evaluate(includes('No flags recorded; this does not mean risk-free')), true);
    }
    results.push('keyboard IPv4, expanded IPv6 and mapped IPv6 use two no-store GETs; zero and unknown retain their meaning');

    await reset('partial'); await submit(); await waitFor(includes('Some source observations failed'));
    await evaluate('document.querySelectorAll(".ip-quality-source").forEach(d=>d.open=true)');
    assert.equal(await evaluate('document.querySelector(".ip-cache-result b")'), null);
    for (const value of ['<b>Fixture source timeout</b>', '"flag": false', '"score": 0', 'Fixture attribution']) assert.equal(await evaluate(includes(value)), true);
    await screenshot('byip-desktop');
    results.push('partial failures, literal error text, raw false/zero and attribution remain inspectable');

    for (const [scenario, message] of [['unavailable', 'Saved source observations are unavailable'], ['failed', 'No source observation succeeded'], ['empty-200', 'No valid cache'], ['expired', 'Cache expired'], ['invalid-date', 'Cache expired']]) {
      await reset(scenario); await submit(); await waitFor(includes(message));
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
      if (scenario === 'unavailable') {
        await evaluate('document.querySelector(".ip-quality-catalog").open=true');
        assert.equal(await evaluate(includes('Enabled')), true);
        assert.equal(await evaluate(includes('does not mean this observation succeeded')), true);
      }
      if (scenario === 'expired' || scenario === 'invalid-date') assert.equal(await evaluate('document.querySelectorAll(".ip-quality-source pre").length'), 0);
    }
    results.push('source unavailable despite available catalog, all failed, empty and expired results remain distinct and hide aggregate facts');

    for (const [scenario, message] of [['missing', 'may be unchecked'], ['invalid-public', 'not supported'], ['unauthorized', 'Access denied'], ['denied', 'Access denied'], ['disabled', 'plugin is disabled'], ['limited', 'Too many reads'], ['server-error', 'Could not read'], ['network', 'Could not read'], ['non-json', 'invalid format'], ['malformed', 'invalid format'], ['malformed-summary', 'invalid format'], ['wrong-ip', 'does not match'], ['via-node', 'invalid format']]) {
      await reset('success'); await submit(); await waitFor(includes('Valid cache read'));
      await evaluate(`fixture.setScenario(${JSON.stringify(scenario)})`); await evaluate(`${read}.click()`);
      await waitFor(includes(message)); await pause(80);
      assert.equal(await evaluate('fixture.requests.length'), 3, scenario + ': no retry or catalog request after result error');
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
    }
    results.push('400/401/403/404/429/503/500, network, non-JSON, malformed, wrong-IP and via-node responses clear older facts without retry');

    for (const [scenario, message] of [['catalog-disabled', 'plugin is disabled'], ['catalog-denied', 'Access denied'], ['catalog-error', 'Valid cache read']]) {
      await reset(scenario); await submit(); await waitFor(includes(message));
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), scenario === 'catalog-error' ? 1 : 0);
      assert.equal(await evaluate('fixture.requests.length'), 2);
      if (scenario === 'catalog-error') {
        await evaluate('document.querySelector(".ip-quality-catalog").open=true');
        assert.equal(await evaluate(includes('Could not read source status')), true);
      }
    }
    results.push('catalog permission/plugin failure suppresses facts; other catalog failure is separate from valid cached observations');

    await reset('expiry'); await submit(); await waitFor(includes('Valid cache read'));
    await waitFor(includes('Cache expired'));
    assert.equal(await evaluate('fixture.requests.length'), 2);
    assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts,.ip-quality-source pre").length'), 0);
    results.push('TTL expires displayed facts without any refresh or check');

    await reset('partial-expiry'); await submit(); await waitFor(includes('Some source observations failed'));
    await waitFor(includes('Cache expired'));
    assert.equal(await evaluate('fixture.requests.length'), 2);
    assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
    assert.equal(await evaluate('document.querySelectorAll(".ip-quality-source pre").length'), 1);
    results.push('one expired source hides aggregate facts while a different fresh source remains inspectable');

    for (const scenario of ['pending', 'catalog-pending']) {
      await reset(scenario); await submit(); await waitFor(`fixture.requests.length===${scenario === 'pending' ? 1 : 2}`);
      await evaluate(`${cancel}.focus()`); await key('Enter', 'Enter'); await waitFor(includes('Reading canceled'));
      assert.equal(await evaluate('fixture.requests.at(-1).aborted'), true);
      assert.equal(await evaluate('document.activeElement.id'), 'ip-cache-address');
    }
    results.push('keyboard cancel aborts both result and catalog stages and restores input focus');

    await reset('pending'); await input('8.8.8.8');
    await evaluate('document.querySelector("form").requestSubmit();document.querySelector("form").requestSubmit()');
    await waitFor('fixture.requests.length===1'); await pause(100);
    assert.equal(await evaluate('fixture.requests.length'), 1);
    await input('1.1.1.1');
    assert.equal(await evaluate('fixture.requests[0].aborted'), true);
    assert.equal(await evaluate(includes('Enter a public IP')), true);
    results.push('duplicate submit stays single-flight; editing cancels and clears the previous query');

    await reset('late'); await submit(); await waitFor('fixture.requests.length===1');
    await input('1.1.1.1'); await input('8.8.8.8'); await evaluate('fixture.setScenario("success")'); await evaluate(`${read}.click()`);
    await waitFor(includes('Valid cache read')); await pause(750);
    assert.equal(await evaluate(includes('Old snapshot')), false);
    assert.equal(await evaluate(includes('Fixture city')), true);
    assert.equal(await evaluate('fixture.requests.length'), 3);
    results.push('A-to-B-to-A identity rejects a late response even when its transport ignores abort');

    await reset('success'); await submit(); await waitFor(includes('Valid cache read'));
    await input('1.1.1.1'); await input('8.8.8.8');
    assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
    assert.equal(await evaluate('fixture.requests.length'), 2);
    results.push('completed A-to-B-to-A input changes do not revive saved results or fetch');

    await reset('pending'); await submit(); await waitFor('fixture.requests.length===1'); await evaluate('fixture.unmount()');
    assert.equal(await evaluate('fixture.requests[0].aborted'), true);
    results.push('component unmount cancels in-flight cache reads');

    await reset('pending'); await submit(); await waitFor('fixture.requests.length===1');
    await evaluate(`document.querySelector('a[href="/resources"]').click()`); await waitFor(`document.querySelector('a[href="/resources/ip-quality"]')`);
    assert.equal(await evaluate(`${queryRequests}[0].aborted`), true);
    await evaluate(`document.querySelector('a[href="/resources/ip-quality"]').focus()`); await key('Enter', 'Enter');
    await waitFor('document.querySelector("#ip-cache-address")');
    assert.equal(await evaluate(`${queryRequests}.length`), 1);
    assert.equal(await evaluate('document.querySelector("#ip-cache-address").value'), '');
    results.push('real resource entry and return links work by keyboard; navigation cancels and return stays idle');

    for (const scenario of ['timeout', 'body-timeout']) {
      await reset(scenario); await submit(); await waitFor(includes('Cache reading timed out'), 23000);
      assert.equal(await evaluate('fixture.requests[0].aborted'), true);
      assert.equal(await evaluate('fixture.requests.length'), 1);
      results.push(scenario + ': actual 20-second deadline covers transport and response body, with no retry');
    }

    await reset('partial'); await submit(); await waitFor(includes('Some source observations failed'));
    const accessibility = await call('Accessibility.getFullAXTree', {});
    assert.ok(accessibility.nodes.some(node => node.role?.value === 'textbox' && node.name?.value === 'Public IP address'));
    assert.ok(accessibility.nodes.some(node => node.role?.value === 'status' && node.name?.value === 'IP cache read status'));
    await evaluate('document.querySelector("#ip-cache-address").focus()'); await key('Tab', 'Tab');
    assert.equal(await evaluate('document.activeElement.textContent'), 'Read cache');
    await evaluate('fixture.locale("zh-CN")'); await waitFor(includes('按 IP 查看质量缓存'));
    await evaluate('document.querySelectorAll(".ip-quality-source").forEach(d=>d.open=true)');
    for (const [width, height] of [[375, 900], [768, 700], [900, 390]]) {
      await call('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: width === 375 });
      await pause(100);
      assert.equal(await evaluate('document.documentElement.scrollWidth<=window.innerWidth'), true, `${width} viewport overflow`);
    }
    await call('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-reduced-motion', value: 'reduce' }] });
    await call('Emulation.setDeviceMetricsOverride', { width: 375, height: 1000, deviceScaleFactor: 1, mobile: true });
    await screenshot('byip-mobile');
    assert.equal(await evaluate('fixture.forbidden.length'), 0);
    assert.equal(exceptions.length, 0, JSON.stringify(exceptions));
    results.push('accessible textbox/status names, keyboard order, Chinese mobile/tablet/landscape layout, reduced motion, zero forbidden fetch or uncaught exception');
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
