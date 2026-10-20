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
  const output = fs.mkdtempSync(path.join(os.tmpdir(), 'resin-batch-task-browser-'));
  const artifacts = process.env.BROWSER_ARTIFACTS_DIR ? path.resolve(process.env.BROWSER_ARTIFACTS_DIR) : output;
  fs.mkdirSync(artifacts, { recursive: true });
  await esbuild.build({ entryPoints: [path.join(__dirname, 'ip-quality-batch-task.fixture.tsx')], outdir: output, entryNames: 'app', bundle: true, loader: { '.woff2': 'file', '.woff': 'file' }, define: { 'import.meta.env.VITE_API_BASE_URL': '""', 'process.env.NODE_ENV': '"production"' }, jsx: 'automatic' });
  fs.writeFileSync(path.join(output, 'index.html'), '<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/app.css"><title>Resin batch task fixture</title><style>body{padding:16px}#root{max-width:1280px;margin:auto}</style><div id="root"></div><script src="/app.js"></script></html>');
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
      throw new Error('Timed out: ' + expression + '\n' + await evaluate('JSON.stringify({status:document.querySelector(".ip-manual-status")?.innerText,requests:fixture.requests,active:document.activeElement?.outerHTML.slice(0,300)})'));
    };
    const includes = text => 'document.body.innerText.includes(' + JSON.stringify(text) + ')';
    const button = text => 'Array.from(document.querySelectorAll("button")).find(b=>b.textContent===' + JSON.stringify(text) + ')';
    const prepareButton = button('Prepare scope and sources'), submitButton = button('Submit this batch');
    const pauseButton = button('Pause page observation'), cancelButton = button('Request server cancellation');
    const readButton = button('Read original task once'), restoreButton = button('Restore query (no resubmission)');
    const clearButton = button('Clear page and prepare a new batch'), retryButton = button('Retry original submission (may execute again)');
    const posts = 'fixture.requests.filter(r=>r.method==="POST")';
    const polls = 'fixture.requests.filter(r=>r.method==="GET"&&r.path.includes("/tasks/"))';
    const input = (selector, value) => evaluate(`(()=>{const el=document.querySelector(${JSON.stringify(selector)});Object.getOwnPropertyDescriptor(el.tagName==='TEXTAREA'?HTMLTextAreaElement.prototype:HTMLInputElement.prototype,'value').set.call(el,${JSON.stringify(value)});el.dispatchEvent(new Event('input',{bubbles:true}));})()`);
    const reset = async mode => { await evaluate(`fixture.scenario(${JSON.stringify(mode)})`); await waitFor('document.querySelector("#batch-hashes")'); };
    const prepare = async (value = 'a'.repeat(32), names = ['db-ip']) => {
      await input('#batch-hashes', value); await evaluate(`${prepareButton}.click()`); await waitFor(includes('Scope ready.'));
      for (const name of names) await evaluate(`document.querySelector('input[value="${name}"]').click()`);
    };
    const submit = async (names = ['db-ip']) => { await prepare('a'.repeat(32), names); await evaluate(`${submitButton}.click()`); };
    const accepted = () => waitFor('document.querySelector("#batch-current-id")');
    const snapshot = async name => { const shot = await call('Page.captureScreenshot', { format: 'png' }); fs.writeFileSync(path.join(artifacts, name + '.png'), Buffer.from(shot.data, 'base64')); };
    const key = async name => { const code = name === 'Tab' ? 9 : 13; await call('Input.dispatchKeyEvent', { type:'keyDown',key:name,code:name,windowsVirtualKeyCode:code,...(name==='Enter'?{text:'\r'}:{}) }); await call('Input.dispatchKeyEvent',{type:'keyUp',key:name,code:name,windowsVirtualKeyCode:code}); };
    await call('Runtime.enable', {}); await call('Page.enable', {});
    await call('Emulation.setDeviceMetricsOverride', { width:1280,height:1100,deviceScaleFactor:1,mobile:false });
    await call('Page.navigate', { url:`http://127.0.0.1:${server.address().port}/` }); await waitFor('document.querySelector("#batch-hashes")');
    assert.equal(await evaluate('fixture.requests.length'),0);
    for (const value of ['a'.repeat(32)+'\ninvalid',Array(51).fill('a'.repeat(32)).join('\n'),'']) {
      await input('#batch-hashes',value); await evaluate(`${prepareButton}.click()`); await waitFor('document.querySelector("#batch-scope-error")'); assert.equal(await evaluate('fixture.requests.length'),0);
    }
    results.push('opening and entire-list validation reject malformed trailing entries and 51 raw duplicate lines before any request');

    await reset('success'); await prepare(Array(50).fill('A'.repeat(32)).join('\n'));
    assert.equal(await evaluate('fixture.requests.length'),1); assert.equal(await evaluate(`${posts}.length`),0);
    assert.equal(await evaluate('document.querySelector(".ip-batch-scope li").textContent'),'a'.repeat(32));
    assert.equal(await evaluate('document.querySelectorAll(".ip-batch-scope li").length'),1);
    assert.equal(await evaluate('document.querySelector("input[value=ipdata]").disabled'),true);
    assert.equal(await evaluate('document.querySelector(".ip-manual-force input").checked'),false);
    results.push('preparation deduplicates an explicitly bounded scope, reads only the catalog and keeps keyed/unavailable sources disabled and force off');

    await evaluate(`${submitButton}.click();${submitButton}.click()`); await accepted();
    assert.equal(await evaluate(`${posts}.length`),1);
    const original = await evaluate(`${posts}[0].body`); assert.deepEqual(original.node_hashes,['a'.repeat(32)]); assert.deepEqual(original.sources,['db-ip']); assert.equal(original.force,false); assert.match(original.request_id,/^[a-f\d]{32}$/);
    assert.equal(await evaluate('fixture.deadlines().includes(300000)'),true);
    results.push('same-turn double submit sends one immutable envelope with a generated request ID, selected sources and default force false');

    await evaluate('fixture.expire(2000)'); await waitFor(includes('Task ended.'));
    assert.equal(await evaluate(`${polls}.length`),1); assert.equal(await evaluate('fixture.deadlines().includes(2000)||fixture.deadlines().includes(300000)'),false);
    assert.equal(await evaluate('document.querySelector(".ip-batch-result").textContent.includes("All sources returned observations")'),true);
    assert.equal(await evaluate('document.querySelector(".ip-batch-result").textContent.includes("2026")'),true);
    results.push('serialized polling stops at completion and renders expired historical observations without presenting a current quality verdict');

    for (const [mode,label] of [['partial','Partial source failure'],['all-failed','All sources failed'],['unavailable','Sources unavailable'],['empty','No source observations'],['failed-item','Execution failed']]) {
      await reset(mode); await submit(['db-ip','egress-consistency']); await accepted(); await evaluate('fixture.expire(2000)'); await waitFor(includes('Task ended.')); assert.equal(await evaluate(includes(label)),true,mode);
      assert.equal(await evaluate('document.querySelectorAll(".ip-batch-result b").length'),0);
    }
    results.push('mixed/failed/unavailable/empty observations and node execution errors remain distinct; remote error markup stays text');

    for (const [mode,label] of [['bad','scope, sources or ID are invalid'],['unauthorized','Task access denied'],['denied','Task access denied'],['missing','task record is unavailable'],['conflict','different scope'],['limited','too frequent'],['busy','task capacity is full'],['server','acceptance is unconfirmed']]) {
      await reset(mode); await submit(); await waitFor(includes(label)); await pause(50); assert.equal(await evaluate(`${posts}.length`),1,mode); assert.equal(await evaluate(`${polls}.length`),0,mode); assert.equal(await evaluate('document.querySelector(".ip-batch-result")===null'),true);
    }
    results.push('400/401/403/404/409/429/503/500 pause with actionable semantics and never automatically retry POST');

    await reset('network'); await submit(); await waitFor(includes('acceptance is unconfirmed'));
    const lost = await evaluate(`${posts}[0].body`); await evaluate('fixture.setScenario("success")'); await evaluate(`${retryButton}.click();${retryButton}.click()`); await accepted();
    assert.equal(await evaluate(`${posts}.length`),2); assert.deepEqual(await evaluate(`${posts}[1].body`),lost); await evaluate(`${pauseButton}.click()`);
    results.push('lost acceptance retains the exact original request ID and full envelope; explicit retry is guarded and warns it may execute again');

    for (const mode of ['wrong-id','wrong-scope']) { await reset('success'); await submit(); await accepted(); await evaluate(`${pauseButton}.click()`); await evaluate(`fixture.setScenario(${JSON.stringify(mode)})`); await evaluate(`${readButton}.click()`); await waitFor(includes('does not match the scope or ID')); assert.equal(await evaluate('document.querySelector(".ip-batch-result")===null'),true); }
    results.push('task ID and original submission scope are both checked before any fetched metadata is displayed');

    await reset('success'); await prepare(); await input('#batch-restore-id','2'.repeat(32)); await evaluate(`${restoreButton}.click()`); await waitFor(includes('Task ended.'));
    assert.equal(await evaluate('document.querySelector("#batch-hashes")===null'),true); assert.equal(await evaluate('document.querySelector(".ip-batch-items code").textContent'),'b'.repeat(32)); assert.equal(await evaluate(`${posts}.length`),0); assert.equal(await evaluate(`${polls}.length`),1);
    await evaluate(`${clearButton}.click()`); await waitFor('document.activeElement?.id==="batch-hashes"'); assert.equal(await evaluate('document.querySelector("#batch-hashes").value'),'');
    results.push('restoring B hides and clears draft A, sends only one GET and reset focuses the newly enabled empty scope field');

    await reset('success'); await input('#batch-restore-id','../tasks/invalid'); await evaluate(`${restoreButton}.click()`); await waitFor('document.querySelector("#batch-id-error")'); assert.equal(await evaluate('fixture.requests.length'),0);
    results.push('invalid restore IDs fail locally without path expansion, catalog reads or new submissions');

    await reset('running'); await submit(); await accepted(); await evaluate('fixture.setScenario("poll-late");fixture.expire(2000)'); await waitFor(`${polls}.length===1`);
    await evaluate(`${cancelButton}.click();${cancelButton}.click()`); await waitFor(includes('Task ended.')); await pause(800);
    assert.equal(await evaluate('document.querySelector(".ip-batch-result-heading").textContent.includes("Canceled")'),true); assert.equal(await evaluate(`${posts}.filter(r=>r.path.endsWith('/actions/cancel')).length`),1); assert.equal(await evaluate(`${polls}[0].aborted`),true);
    results.push('server cancel aborts and invalidates an older poll; its late running response cannot overwrite confirmed cancellation');

    await reset('submit-late'); await submit(); await waitFor(`${posts}.length===1`); await evaluate(`${clearButton}.click()`); await waitFor('document.activeElement?.id==="batch-hashes"');
    await input('#batch-hashes','b'.repeat(32)); await input('#batch-hashes','a'.repeat(32)); await pause(800);
    assert.equal(await evaluate('document.querySelector("#batch-current-id")===null'),true); assert.equal(await evaluate(`${posts}.length`),1); assert.equal(await evaluate(`${posts}[0].aborted`),true);
    results.push('clearing and editing A to B to A invalidates a late acceptance response without silently resubmitting');

    await reset('running'); await submit(); await accepted(); await evaluate(`${pauseButton}.click();fixture.setScenario('body-timeout')`); await evaluate(`${readButton}.click()`); await waitFor(`${polls}.length===1`); await pause(50);
    assert.equal(await evaluate('fixture.deadlines().includes(2000)'),false); await evaluate('fixture.expire(20000)'); await waitFor(includes('task request timed out')); assert.equal(await evaluate(`${polls}[0].aborted`),true);
    results.push('the read deadline includes response-body parsing and a pending body never starts an overlapping poll');

    await reset('running'); await submit(); await accepted(); await evaluate('fixture.expire(300000)'); await waitFor(includes('five-minute observation ended')); assert.equal(await evaluate('fixture.deadlines().includes(2000)'),false); assert.equal(await evaluate(`${posts}.length`),1);
    results.push('the five-minute observation budget stops GET scheduling without sending cancellation or another submission');

    await reset('pending'); await submit(); await waitFor(`${posts}.length===1`); await evaluate(`${pauseButton}.click()`); await waitFor(includes('acceptance is unconfirmed')); assert.equal(await evaluate(`${posts}.length`),1); assert.equal(await evaluate(`${posts}[0].aborted`),true);
    await reset('running'); await submit(); await accepted(); await evaluate(`document.querySelector('a[href="/plugins"]').click()`); await waitFor(includes('Fixture plugins')); assert.equal(await evaluate('fixture.deadlines().includes(2000)||fixture.deadlines().includes(300000)'),false); assert.equal(await evaluate(`${posts}.length`),1);
    results.push('pausing an unknown acceptance and navigating away abort only page observation; neither implies server cancellation');

    await reset('success'); await input('#batch-restore-id','1'.repeat(32)); await evaluate(`${restoreButton}.click()`); await waitFor(includes('Task ended.')); await evaluate('fixture.setScenario("denied")'); await evaluate(`${readButton}.click()`); await waitFor(includes('Task access denied')); assert.equal(await evaluate('document.querySelector(".ip-batch-result")===null'),true);
    await reset('running'); await submit(); await accepted(); await evaluate('fixture.setScenario("limited");fixture.expire(2000)'); await waitFor(includes('too frequent')); assert.equal(await evaluate('fixture.deadlines().includes(2000)'),false);
    results.push('auth loss clears previously displayed task details; throttling pauses polling and requires manual retry');

    await reset('success'); await prepare(); await evaluate(`${submitButton}.focus()`); await key('Enter'); await accepted(); await evaluate('fixture.expire(2000)'); await waitFor(includes('Task ended.')); await evaluate(`${clearButton}.click()`); await waitFor('document.activeElement?.id==="batch-hashes"');
    await key('Tab'); assert.equal(await evaluate('document.activeElement.textContent'),'Prepare scope and sources');
    await evaluate('fixture.scenario("success","/resources/ip-quality-check")'); await waitFor('document.querySelector("#manual-subject")'); await evaluate(`document.querySelector('a[href="/resources/ip-quality-batch"]').focus()`); await key('Enter'); await waitFor('document.querySelector("#batch-hashes")'); assert.equal(await evaluate('fixture.requests.length'),0);
    results.push('keyboard submit/reset order and the real manual-page navigation link work without implicit API calls');

    for (const [mode,names,label] of [['suppressed',['egress-consistency'],'Selected sources locally limited'],['partial-suppression',['db-ip','egress-consistency'],'Some sources locally limited']]) {
      await reset(mode); await submit(names); await accepted(); await evaluate('fixture.expire(2000)'); await waitFor(includes('Task ended.'));
      await evaluate('document.querySelectorAll(".ip-batch-items details").forEach(d=>d.open=true)');
      assert.equal(await evaluate(includes(label)),true); assert.equal(await evaluate(includes('Source probe is still in progress')),true);
      assert.equal(await evaluate('document.querySelectorAll(".ip-batch-source").length'),names.length-1);
      assert.equal(await evaluate('document.querySelector(".ip-batch-items").textContent.includes("Earliest reevaluation time")'),false);
      if(mode==='suppressed') assert.equal(await evaluate('document.querySelectorAll(".ip-batch-items details > p").length'),3);
      assert.equal(await evaluate('fixture.deadlines().includes(2000)||fixture.deadlines().includes(300000)'),false);
      await pause(75); assert.equal(await evaluate(`${posts}.length`),1);
      if(mode==='partial-suppression') {
        await call('Emulation.setDeviceMetricsOverride',{width:375,height:1100,deviceScaleFactor:1,mobile:true});
        await evaluate('document.querySelector(".ip-batch-result").scrollIntoView()'); await snapshot('batch-suppression-mobile');
        assert.equal(await evaluate('document.documentElement.scrollWidth<=window.innerWidth'),true);
        await call('Emulation.setDeviceMetricsOverride',{width:1280,height:1100,deviceScaleFactor:1,mobile:false});
      }
    }
    results.push('completed full and partial suppression preserve separate lifecycle, bounded reasons and absent observation/probe retry time without resubmission');

    await reset('all-failed'); await submit(['db-ip','egress-consistency']); await accepted(); await evaluate('fixture.expire(2000)'); await waitFor(includes('Task ended.')); await evaluate('document.querySelectorAll(".ip-batch-items details").forEach(d=>d.open=true)');
    await snapshot('batch-task-desktop'); await evaluate('fixture.locale("zh-CN")'); await waitFor(includes('批量 IP 质量任务'));
    for (const [width,height] of [[375,900],[768,700],[900,390]]) { await call('Emulation.setDeviceMetricsOverride',{width,height,deviceScaleFactor:1,mobile:width===375}); await pause(100); assert.equal(await evaluate('document.documentElement.scrollWidth<=window.innerWidth'),true,`${width} viewport overflow`); }
    await call('Emulation.setEmulatedMedia',{features:[{name:'prefers-reduced-motion',value:'reduce'}]}); await call('Emulation.setDeviceMetricsOverride',{width:375,height:1100,deviceScaleFactor:1,mobile:true}); await evaluate('window.scrollTo(0,0)'); await snapshot('batch-task-mobile'); await evaluate('document.querySelector(".ip-batch-result").scrollIntoView()'); await snapshot('batch-task-result-mobile');
    assert.equal(await evaluate('fixture.forbidden.length'),0); assert.equal(exceptions.length,0,JSON.stringify(exceptions));
    results.push('Chinese and English metadata, mobile/tablet/landscape and reduced motion render without overflow, forbidden fetches or uncaught exceptions');
    fs.writeFileSync(path.join(artifacts,'browser-results.json'),JSON.stringify({passed:true,checks:results},null,2));
    console.log(JSON.stringify({passed:true,checks:results,artifacts},null,2));
  } finally {
    if (command) await command('Browser.close').catch(()=>{});
    if (socket) socket.close(); server.close(); browser.kill();
  }
}
main().catch(error=>{console.error(error);process.exitCode=1;});
