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
  const output = fs.mkdtempSync(path.join(os.tmpdir(), 'resin-manual-browser-'));
  const artifacts = process.env.BROWSER_ARTIFACTS_DIR ? path.resolve(process.env.BROWSER_ARTIFACTS_DIR) : output;
  fs.mkdirSync(artifacts, { recursive: true });
  await esbuild.build({ entryPoints: [path.join(__dirname, 'ip-quality-manual.fixture.tsx')], outdir: output, entryNames: 'app', bundle: true, loader: { '.woff2': 'file', '.woff': 'file' }, define: { 'import.meta.env.VITE_API_BASE_URL': '""', 'process.env.NODE_ENV': '"production"' }, jsx: 'automatic' });
  fs.writeFileSync(path.join(output, 'index.html'), '<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/app.css"><title>Resin manual check fixture</title><style>body{padding:16px}#root{max-width:1280px;margin:auto}</style><div id="root"></div><script src="/app.js"></script></html>');
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
      while (Date.now() - start < timeout) { if (await evaluate(`Boolean(${expression})`)) return; await pause(50); }
      throw new Error('Timed out: ' + expression + '\n' + await evaluate('JSON.stringify({status:document.querySelector(".ip-manual-status")?.innerText,requests:fixture.requests,active:document.activeElement?.outerHTML.slice(0,300)})'));
    };
    const includes = text => 'document.body.innerText.includes(' + JSON.stringify(text) + ')';
    const button = text => 'Array.from(document.querySelectorAll("button")).find(b=>b.textContent===' + JSON.stringify(text) + ')';
    const load = button('Load object and sources'), run = button('Run one check'), stop = button('Stop waiting'), cache = button('Read current cache to verify');
    const posts = 'fixture.requests.filter(r=>r.method==="POST")';
    const key = async name => {
      const code = name === 'Tab' ? 9 : 13;
      await call('Input.dispatchKeyEvent', { type: 'keyDown', key: name, code: name, windowsVirtualKeyCode: code, ...(name === 'Enter' ? { text: '\r' } : {}) });
      await call('Input.dispatchKeyEvent', { type: 'keyUp', key: name, code: name, windowsVirtualKeyCode: code });
    };
    const input = value => evaluate('(()=>{const el=document.querySelector("#manual-subject");Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,"value").set.call(el,' + JSON.stringify(value) + ');el.dispatchEvent(new Event("input",{bubbles:true}));})()');
    const reset = async (scenario, node = false) => {
      await evaluate(`fixture.scenario(${JSON.stringify(scenario)},${node ? '"/resources/ip-quality-check?node="+fixture.hash' : '"/resources/ip-quality-check"'})`);
      await waitFor('document.querySelector("#manual-subject")');
    };
    const prepare = async (node = false, ip = '8.8.8.8') => {
      if (!node) await input(ip);
      await evaluate(`${load}.click()`);
      await waitFor(includes('Object ready'));
    };
    const select = async (names = ['db-ip']) => {
      for (const name of names) await evaluate(`document.querySelector('input[value="${name}"]').click()`);
    };
    const submit = async (node = false, names = ['db-ip'], ip = '8.8.8.8') => { await prepare(node, ip); await select(names); await evaluate(`${run}.click()`); };
    const screenshot = async name => { const shot = await call('Page.captureScreenshot', { format: 'png' }); fs.writeFileSync(path.join(artifacts, name + '.png'), Buffer.from(shot.data, 'base64')); };
    await call('Runtime.enable', {}); await call('Page.enable', {});
    await call('Emulation.setDeviceMetricsOverride', { width: 1280, height: 1100, deviceScaleFactor: 1, mobile: false });
    await call('Page.navigate', { url: `http://127.0.0.1:${server.address().port}/` });
    await waitFor('document.querySelector("#manual-subject")');

    for (const entry of ['/resources', '/nodes']) {
      await evaluate(`fixture.scenario("success",${JSON.stringify(entry)})`);
      await waitFor(`document.querySelector('a[href^="/resources/ip-quality-check"]')`);
      await evaluate(`document.querySelector('a[href^="/resources/ip-quality-check"]').focus()`); await key('Enter');
      await waitFor('document.querySelector("#manual-subject")');
      assert.equal(await evaluate('document.querySelector("#manual-kind").value'), entry === '/nodes' ? 'node' : 'ip');
      assert.equal(await evaluate('document.querySelector("#manual-subject").value'), entry === '/nodes' ? 'a'.repeat(32) : '');
      assert.equal(await evaluate('fixture.requests.filter(r=>r.path!=="/api/v1/geoip/status").length'), 0);
    }
    results.push('real resource and node-panel links open the correct mode by keyboard without starting checks or reading sources');
    await reset('success');

    await input('8.8.8.8');
    await evaluate('window.dispatchEvent(new Event("focus"));window.dispatchEvent(new Event("online"))');
    await pause(100);
    assert.equal(await evaluate('fixture.requests.length'), 0);
    for (const value of ['', 'sources', 'lookup', 'example.com', 'https://8.8.8.8', '8.8.8.8:443', '8.8.8.8/32', 'fe80::1%eth0', '999.8.8.8']) {
      await input(value); await evaluate(`${load}.click()`);
      assert.equal(await evaluate('document.querySelector("#manual-subject").getAttribute("aria-invalid")'), 'true');
      assert.equal(await evaluate('document.activeElement.id'), 'manual-subject');
      assert.equal(await evaluate('fixture.requests.length'), 0);
    }
    results.push('entry, edits, focus/reconnect and invalid input never fetch; inline validation focuses the object');

    for (const [address, canonical] of [['8.8.8.8', '8.8.8.8'], ['2001:4860:4860:0:0:0:0:8888', '2001:4860:4860::8888'], ['::ffff:8.8.8.8', '8.8.8.8']]) {
      await reset('success'); await input(address); await evaluate('document.querySelector("#manual-subject").focus()'); await key('Enter');
      await waitFor(includes('Object ready'));
      assert.deepEqual(await evaluate('fixture.requests.map(r=>[r.method,r.path])'), [['GET', '/api/v1/ip-checks/sources']]);
      assert.equal(await evaluate(`${run}.disabled`), true);
      assert.equal(await evaluate('document.querySelectorAll("input:checked").length'), 0);
      assert.equal(await evaluate('document.querySelector("input[value=ipdata]").disabled'), true);
      assert.equal(await evaluate('document.querySelector("input[value=egress-consistency]")'), null);
      await select(); await evaluate(`${run}.click()`); await waitFor(includes('Source observations returned'));
      assert.deepEqual(await evaluate(`${posts}.map(r=>[r.path,r.body])`), [['/api/v1/ip-checks/lookup', { ip: canonical, sources: ['db-ip'], force: false }]]);
      assert.equal(await evaluate('fixture.requests.every(r=>r.cache==="no-store")'), true);
      assert.equal(await evaluate('fixture.deadlines().length'), 0);
      assert.equal(await evaluate(includes('0 / Unknown')), true);
      for (const text of ['ChatGPT', 'Claude', 'Cloudflare', 'Egress consistency']) assert.equal(await evaluate('document.querySelector(".ip-quality-facts").innerText.includes(' + JSON.stringify(text) + ')'), false);
    }
    results.push('IPv4/IPv6/mapped input prepares with GET only; explicit nonempty sources and force=false yield exactly one normalized lookup POST');

    await reset('success'); await prepare(); await select();
    await evaluate('document.querySelector(".ip-manual-force input").click()');
    assert.equal(await evaluate(includes('may still join in-flight checks')), true);
    await evaluate(`${run}.click()`); await waitFor(includes('Source observations returned'));
    assert.equal(await evaluate(`${posts}[0].body.force`), true);
    const postCount = await evaluate(`${posts}.length`);
    await evaluate(`${cache}.click()`); await waitFor(includes('Current cache response'));
    assert.equal(await evaluate(`${posts}.length`), postCount);
    assert.equal(await evaluate('fixture.requests.at(-1).path'), '/api/v1/ip-checks/8.8.8.8');
    results.push('force is explicit and explains in-flight sharing; verification reads cache with GET without another check');

    await reset('partial', true); await submit(true, ['db-ip', 'egress-consistency']);
    await waitFor(includes('Some sources failed'));
    assert.deepEqual(await evaluate('fixture.requests.map(r=>r.method)'),'GET GET GET POST GET'.split(' '));
    assert.equal(await evaluate(`${posts}[0].path`), '/api/v1/nodes/' + 'a'.repeat(32) + '/actions/check-ip');
    assert.deepEqual(await evaluate(`${posts}[0].body`), { sources: ['db-ip', 'egress-consistency'], force: false });
    await evaluate('document.querySelectorAll(".ip-quality-source").forEach(d=>d.open=true)');
    for (const text of ['<b>Fixture source timeout</b>', '"flag": false', '"score": 0', 'Fixture attribution', 'Targets can see its egress IP']) assert.equal(await evaluate(includes(text)), true);
    assert.equal(await evaluate('document.querySelector(".ip-manual-result b")'), null);
    await screenshot('manual-desktop');
    results.push('node check verifies identity before and after POST; partial failures, literal error text, raw evidence and external-target disclosure remain visible');

    for (const [scenario, message] of [['unavailable', 'Selected sources are unavailable'], ['failed', 'No source succeeded'], ['expired', 'Some observations expired']]) {
      await reset(scenario); await submit(); await waitFor(includes(message));
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
      if (scenario === 'expired') assert.equal(await evaluate('document.querySelectorAll(".ip-quality-source pre").length'), 0);
    }
    results.push('unavailable, all-failed and expired observations never show aggregate success facts');

    for (const [scenario, message] of [['bad', 'Invalid address'], ['unauthorized', 'Not authorized'], ['denied', 'Not authorized'], ['missing', 'Node or object not found'], ['conflict', 'Node egress is unavailable'], ['limited', 'limited request frequency'], ['busy', 'plugin is unavailable'], ['server-error', 'completion is unknown'], ['network', 'completion is unknown'], ['non-json', 'did not match'], ['malformed-summary', 'did not match'], ['wrong-ip', 'did not match'], ['wrong-source', 'did not match'], ['empty', 'did not match']]) {
      await reset('success'); await submit(); await waitFor(includes('Source observations returned'));
      await evaluate(`fixture.setScenario(${JSON.stringify(scenario)})`); await evaluate(`${run}.click()`); await waitFor(includes(message)); await pause(40);
      assert.equal(await evaluate(`${posts}.length`), 2, scenario);
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0, scenario);
    }
    results.push('400/401/403/404/409/429/503/500, network and malformed/object/source responses clear older results without retries');

    for (const [scenario, message] of [['disabled', 'plugin is unavailable'], ['catalog-denied', 'Not authorized'], ['bad-catalog', 'Invalid source catalog'], ['no-sources', 'No selectable sources']]) {
      await reset(scenario); await input('8.8.8.8'); await evaluate(`${load}.click()`); await waitFor(includes(message));
      assert.equal(await evaluate(`${posts}.length`), 0);
      assert.equal(await evaluate(`${run}?.disabled ?? true`), true);
    }
    await reset('no-egress', true); await evaluate(`${load}.click()`); await waitFor(includes('node lacks a valid egress'));
    assert.equal(await evaluate('fixture.requests.length'), 1);
    results.push('disabled/denied/invalid catalogs, no selectable sources and missing node egress cannot start a check');

    for (const scenario of ['changed-before', 'changed-after', 'version-after']) {
      await reset(scenario, true); await submit(true); await waitFor(includes('Node egress or version changed'));
      assert.equal(await evaluate(`${posts}.length`), scenario === 'changed-before' ? 0 : 1);
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
    }
    results.push('changed egress before execution prevents POST; changed egress/version after execution hides results');

    await reset('pending'); await prepare(); await select();
    await evaluate(`${run}.click();${run}.click()`); await waitFor(`${posts}.length===1`);
    assert.equal(await evaluate('document.querySelector("fieldset").disabled'), true);
    assert.equal(await evaluate('document.querySelector(".ip-manual-force input").disabled'), true);
    await evaluate(`${stop}.focus()`); await key('Enter'); await waitFor(includes('Stopped waiting'));
    assert.equal(await evaluate(`${posts}[0].aborted`), true);
    assert.equal(await evaluate('document.activeElement.id'), 'manual-subject');
    assert.equal(await evaluate('fixture.deadlines().length'), 0);
    assert.equal(await evaluate(`${posts}.length`), 1);
    results.push('double click starts one POST; selection/force lock while busy; keyboard stop aborts waiting and restores focus');

    await reset('pending'); await submit(); await waitFor(`${posts}.length===1`);
    await evaluate('(()=>{const el=document.querySelector("#manual-kind");el.value="node";el.dispatchEvent(new Event("change",{bubbles:true}));})()');
    assert.equal(await evaluate(`${posts}[0].aborted`), true);
    assert.equal(await evaluate('document.querySelector("#manual-subject").value'), '');
    assert.equal(await evaluate('document.querySelector("fieldset")'), null);
    await input('a'.repeat(31)); await evaluate(`${load}.click()`);
    assert.equal(await evaluate('document.querySelector("#manual-subject").getAttribute("aria-invalid")'), 'true');
    assert.equal(await evaluate('fixture.requests.length'), 2);
    results.push('switching object type cancels and clears preparation; incomplete node identity never fetches');

    for (const scenario of ['prepare-pending', 'verify-pending']) {
      await reset(scenario, scenario === 'verify-pending');
      if (scenario === 'prepare-pending') { await input('8.8.8.8'); await evaluate(`${load}.click()`); await waitFor('fixture.requests.length===1'); }
      else { await submit(true); await waitFor('fixture.requests.length===5'); }
      await evaluate(`${stop}.click()`); await waitFor(includes('Stopped waiting'));
      assert.equal(await evaluate('fixture.requests.at(-1).aborted'), true);
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
    }
    results.push('stop aborts source preparation and post-check node verification without showing a result');

    await reset('late'); await submit(); await waitFor(`${posts}.length===1`);
    await input('1.1.1.1'); await input('8.8.8.8'); await evaluate('fixture.setScenario("success")');
    await submit(); await waitFor(includes('Source observations returned')); await pause(800);
    assert.equal(await evaluate(includes('Old response')), false);
    assert.equal(await evaluate(includes('Fixture city')), true);
    assert.equal(await evaluate(`${posts}.length`), 2);
    await input('1.1.1.1'); await input('8.8.8.8');
    assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
    results.push('editing aborts; A-to-B-to-A rejects late transports and never revives completed results');

    await reset('pending'); await submit(); await waitFor(`${posts}.length===1`);
    await evaluate(`document.querySelector('a[href="/plugins"]').click()`); await waitFor(includes('Fixture plugins'));
    assert.equal(await evaluate(`${posts}[0].aborted`), true);
    await evaluate('document.querySelector("a").click()'); await waitFor('document.querySelector("#manual-subject")');
    assert.equal(await evaluate(`${posts}.length`), 1);
    assert.equal(await evaluate('document.querySelector("#manual-subject").value'), '');
    await reset('pending'); await submit(); await waitFor(`${posts}.length===1`); await evaluate('fixture.unmount()');
    assert.equal(await evaluate(`${posts}[0].aborted`), true);
    assert.equal(await evaluate('fixture.deadlines().length'), 0);
    await reset('body-timeout'); await submit(); await waitFor(`${posts}.length===1`); await pause(50);
    await evaluate(`${stop}.click()`); await waitFor(includes('Stopped waiting'));
    assert.equal(await evaluate(`${posts}[0].aborted`), true);
    assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'), 0);
    results.push('navigation, unmount and response-body cancellation abort; return stays idle without repeated POST');

    for (const scenario of ['timeout', 'body-timeout', 'prepare-pending']) {
      await reset(scenario);
      if (scenario === 'prepare-pending') { await input('8.8.8.8'); await evaluate(`${load}.click()`); }
      else await submit();
      await waitFor(`fixture.requests.length===${scenario === 'prepare-pending' ? 1 : 2}`);
      if (scenario === 'body-timeout') await pause(50);
      const ms = scenario === 'prepare-pending' ? 20000 : 130000;
      assert.deepEqual(await evaluate('fixture.deadlines()'), [ms]);
      await evaluate(`fixture.expireWait(${ms})`);
      await waitFor(includes(scenario === 'prepare-pending' ? 'Reading timed out' : 'Waiting timed out'));
      assert.equal(await evaluate('fixture.requests.at(-1).aborted'), true);
      assert.equal(await evaluate(`${posts}.length`), scenario === 'prepare-pending' ? 0 : 1);
      assert.equal(await evaluate('fixture.deadlines().length'), 0);
    }
    results.push('explicitly simulated 130-second transport/body and 20-second preparation deadlines abort without retries; no wall-time claim');

    await reset('conflict'); await submit(); await waitFor(includes('Node egress is unavailable'));
    await evaluate('fixture.setScenario("success")'); await evaluate(`${cache}.click()`); await waitFor(includes('Current cache response'));
    assert.equal(await evaluate(`${posts}.length`), 1);
    assert.equal(await evaluate(includes('Fixture city')), true);
    await reset('success'); await prepare(); await evaluate(`${cache}.click()`); await waitFor(includes('may be unchecked'));
    assert.equal(await evaluate(`${posts}.length`), 0);
    results.push('failed POST can leave readable cache; explicit verification distinguishes absent cache and does not promise rollback or persistence');

    await reset('expiry'); await submit(); await waitFor(includes('Source observations returned'));
    await waitFor(includes('Some observations expired'));
    assert.equal(await evaluate('fixture.requests.length'), 2);
    assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts,.ip-quality-source pre").length'), 0);
    results.push('displayed evidence expires without an automatic read or check');

    for (const [mode,names,label] of [['suppressed',['egress-consistency'],'Selected sources were locally limited'],['partial-suppression',['db-ip','egress-consistency'],'Some sources were locally limited']]) {
      await reset(mode,true); await submit(true,names); await waitFor(includes(label));
      assert.equal(await evaluate(includes('Local source request limits')),true);
      assert.equal(await evaluate(includes('Source daily budget exhausted')),true);
      assert.equal(await evaluate(includes('force cannot bypass source limits')),true);
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-source pre").length'),names.length-1);
      assert.equal(await evaluate('document.querySelectorAll(".ip-quality-facts").length'),names.length-1);
      await pause(75); assert.equal(await evaluate(`${posts}.length`),1);
      if (mode==='suppressed') {
        assert.equal(await evaluate('document.querySelector(".ip-manual-result > p").textContent'),'8.8.8.8');
        await evaluate(`${cache}.click()`); await waitFor(includes('may be unchecked')); assert.equal(await evaluate(`${posts}.length`),1);
      }
      if (mode==='partial-suppression') {
        await call('Emulation.setDeviceMetricsOverride',{width:375,height:1100,deviceScaleFactor:1,mobile:true});
        await evaluate('document.querySelector(".ip-manual-result").scrollIntoView()'); await screenshot('manual-suppression-mobile');
        assert.equal(await evaluate('document.documentElement.scrollWidth<=window.innerWidth'),true);
        await call('Emulation.setDeviceMetricsOverride',{width:1280,height:1100,deviceScaleFactor:1,mobile:false});
      }
    }
    results.push('full and partial local suppression stay separate from observations, contain bounded reasons, do not auto-retry and do not invent cache or observation time');

    await reset('partial', true); await submit(true, ['db-ip', 'egress-consistency']); await waitFor(includes('Some sources failed'));
    const ax = await call('Accessibility.getFullAXTree', {});
    assert.ok(ax.nodes.some(node => node.role?.value === 'textbox' && node.name?.value === 'Node hash (32 hexadecimal characters)'));
    assert.ok(ax.nodes.some(node => node.role?.value === 'status' && node.name?.value === 'Manual check status'));
    await evaluate('document.querySelector("#manual-subject").focus()'); await key('Tab');
    assert.equal(await evaluate('document.activeElement.textContent'), 'Load object and sources');
    await evaluate('fixture.locale("zh-CN")'); await waitFor(includes('手动 IP 质量检测'));
    await evaluate('document.querySelectorAll(".ip-quality-source").forEach(d=>d.open=true)');
    await evaluate('document.querySelector(".ip-manual-result").scrollIntoView()'); await screenshot('manual-result-desktop');
    await evaluate('window.scrollTo(0,0)');
    for (const [width, height] of [[375, 900], [768, 700], [900, 390]]) {
      await call('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: width === 375 }); await pause(100);
      assert.equal(await evaluate('document.documentElement.scrollWidth<=window.innerWidth'), true, `${width} viewport overflow`);
    }
    await call('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-reduced-motion', value: 'reduce' }] });
    await call('Emulation.setDeviceMetricsOverride', { width: 375, height: 1100, deviceScaleFactor: 1, mobile: true }); await screenshot('manual-mobile');
    await evaluate('document.querySelector(".ip-manual-result").scrollIntoView()'); await screenshot('manual-result-mobile');
    assert.equal(await evaluate('fixture.forbidden.length'), 0);
    assert.equal(exceptions.length, 0, JSON.stringify(exceptions));
    results.push('accessible object/status names, keyboard order, Chinese mobile/tablet/landscape and reduced motion with no forbidden fetch or uncaught exception');
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
