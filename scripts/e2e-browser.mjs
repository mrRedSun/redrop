#!/usr/bin/env node
// End-to-end encrypted links in a real browser (headless Chromium over the
// DevTools protocol, no packages), for scripts/e2e.sh. DESIGN.md
// "End-to-end encrypted links"; review 33 B-2, M1 and m-A.
//
//   node scripts/e2e-browser.mjs links.json
//
// links.json holds {probe, password, once, svg}: {id, url} each (url is the
// full link), plus password.password. The probe is scripts/e2e-browser-probe.html
// shared as a folder with docs/two.html. Set REDROP_CLI (with REDROP_CONFIG)
// to also check that revoking stops an open page. Each check prints ok or
// FAIL; the exit code is 1 when any failed. Links stay out of the output.
// Plain-HTTP content origins are marked secure for this browser only
// (--unsafely-treat-insecure-origin-as-secure), since browsers give WebCrypto
// and service workers to secure contexts alone.
import { spawn, execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const links = JSON.parse(readFileSync(process.argv[2], 'utf8'))
const keyOf = (l) => l.split('#k=')[1]
const secrets = Object.values(links).map((l) => keyOf(l.url)).filter(Boolean)
const hide = (s) => secrets.reduce((t, k) => t.replaceAll(k, '<KEY>'), String(s))
const origins = [...new Set(Object.values(links).map((l) => new URL(l.url).origin))]
  .filter((o) => o.startsWith('http:') && !/\/\/(localhost|[^/]*\.localhost)(:|$)/.test(o))
const profile = mkdtempSync(join(tmpdir(), 'redrop-e2e-chromium-'))
const args = ['--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--remote-debugging-port=0', `--user-data-dir=${profile}`]
if (origins.length) args.push(`--unsafely-treat-insecure-origin-as-secure=${origins.join(',')}`)
const chrome = spawn(process.env.CHROMIUM || 'chromium', [...args, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let failed = 0
const check = (name, ok, detail = '') => {
  if (!ok) failed++
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}${detail ? ': ' + hide(detail).slice(0, 200) : ''}`)
}

function connect(url) {
  const ws = new WebSocket(url)
  let id = 0
  const pending = new Map()
  const listeners = []
  ws.addEventListener('message', (e) => {
    const msg = JSON.parse(e.data)
    if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id) }
    if (msg.method) listeners.forEach((f) => f(msg))
  })
  const ready = new Promise((r) => ws.addEventListener('open', r))
  const send = (method, params = {}) => new Promise((resolve) => { const i = ++id; pending.set(i, resolve); ws.send(JSON.stringify({ id: i, method, params })) })
  return { ws, ready, send, on: (f) => listeners.push(f) }
}

let browserWS, port
// visitor opens a fresh browser context: its own storage, like a new visitor.
async function visitor() {
  const b = connect(browserWS)
  await b.ready
  const ctx = (await b.send('Target.createBrowserContext')).result.browserContextId
  const targetId = (await b.send('Target.createTarget', { url: 'about:blank', browserContextId: ctx })).result.targetId
  const p = connect(`ws://127.0.0.1:${port}/devtools/page/${targetId}`)
  await p.ready
  const requests = []
  p.on((msg) => {
    if (msg.method === 'Network.requestWillBeSent') requests.push(msg.params.request.url + ' ' + (msg.params.request.headers.Referer ?? ''))
  })
  await p.send('Page.enable')
  await p.send('Runtime.enable')
  await p.send('Network.enable')
  const evaluate = async (expression) => (await p.send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true })).result?.result?.value
  // waitFor polls an expression; the bound only catches a hang.
  const waitFor = async (expression, ms = 30000) => {
    for (let t = 0; t < ms; t += 200) { if (await evaluate(expression)) return true; await sleep(200) }
    return false
  }
  return { ...p, evaluate, waitFor, requests, close: () => b.send('Target.disposeBrowserContext', { browserContextId: ctx }) }
}
const frameText = `(() => { try { return document.getElementById('rd-frame').contentDocument.body.innerText } catch (e) { return '' } })()`
const frameData = `(() => { try { return JSON.stringify(document.getElementById('rd-frame').contentDocument.body.dataset) } catch (e) { return '{}' } })()`
const pageText = `document.body ? document.body.innerText : ''`

try {
  browserWS = await new Promise((resolve, reject) => {
    let buf = ''
    chrome.stderr.on('data', (d) => { buf += d; const m = /DevTools listening on (ws:\/\/\S+)/.exec(buf); if (m) resolve(m[1]) })
    setTimeout(() => reject(new Error('chromium did not start: ' + buf)), 30000)
  })
  port = new URL(browserWS).port

  // The key, against everything a script in the artifact can read (B-2).
  {
    const key = keyOf(links.probe.url)
    const v = await visitor()
    await v.send('Page.navigate', { url: links.probe.url })
    check('the probe page decrypts in the frame', await v.waitFor(`${frameText}.includes('probe-4471') && JSON.parse(${frameData}).done === '1'`))
    const found = JSON.parse((await v.evaluate(frameData)) || '{}')
    for (const [route, value] of Object.entries(found)) {
      if (route === 'done') continue
      check(`a script in the artifact finds no key in ${route}`, !String(value).includes(key) && !String(value).includes('#k='), String(value))
    }
    check('the stored key is a non-extractable CryptoKey', /^\[\{"type":"secret","extractable":false,"algorithm":"HKDF"\}\]$/.test(found.storedKeys ?? ''), found.storedKeys)
    check('the artifact can\'t export the stored key', String(found.exportKey).startsWith('refused'), found.exportKey)
    const href = await v.evaluate('location.href')
    check('the address bar has no key', !href.includes(key) && href.endsWith('/#/'), href)
    await v.evaluate(`document.getElementById('rd-frame').contentDocument.getElementById('rel').click()`)
    check('a relative link opens the next decrypted page', await v.waitFor(`${frameText}.includes('two-8842')`))
    await v.waitFor(`location.hash === '#/docs/two.html#sec'`, 5000)
    check('the address bar follows in the fragment', (await v.evaluate('location.hash')) === '#/docs/two.html#sec', await v.evaluate('location.href'))
    await v.send('Page.reload', {})
    check('a reload opens the same page with the stored key', await v.waitFor(`${frameText}.includes('two-8842')`))
    await v.send('Page.reload', { ignoreCache: true })
    check('a hard reload too', await v.waitFor(`${frameText}.includes('two-8842')`))
    const leaked = v.requests.filter((r) => r.includes(key) || r.includes('#k='))
    check('no request carried the key in its URL or Referer', leaked.length === 0, `${leaked.length} of ${v.requests.length}`)
    await v.close()
  }

  // Gates keep the key (M1): the password, then a one-time link.
  {
    const v = await visitor()
    await v.send('Page.navigate', { url: links.password.url })
    check('the password gate is shown', await v.waitFor(`!!document.querySelector('input[name=password]')`))
    await v.evaluate(`(() => { document.querySelector('input[name=password]').value = ${JSON.stringify(links.password.password)}; document.querySelector('button[type=submit]').click() })()`)
    check('after the right password the page decrypts', await v.waitFor(`${frameText}.includes('pw-page-5150')`), await v.evaluate(pageText))
    check('and the key left the address bar', !(await v.evaluate('location.href')).includes('#k='))
    await v.close()
  }
  {
    const v = await visitor()
    await v.send('Page.navigate', { url: links.once.url })
    check('the one-time gate is shown', await v.waitFor(`!!document.querySelector('button[type=submit]')`))
    await v.evaluate(`document.querySelector('button[type=submit]').click()`)
    check('after Open the one-time page decrypts', await v.waitFor(`${frameText}.includes('once-page-6160')`), await v.evaluate(pageText))
    await v.close()
    const second = await visitor()
    await second.send('Page.navigate', { url: links.once.url })
    check('a second visitor gets the miss', await second.waitFor(`(${pageText}).includes("doesn't exist")`), await second.evaluate(pageText))
    await second.close()
  }

  // An SVG file artifact is shown in an <img>, where its script can't run (m-A).
  {
    const v = await visitor()
    await v.send('Page.navigate', { url: links.svg.url })
    const shown = await v.waitFor(`(() => { try { const d = document.getElementById('rd-frame').contentDocument; return d.contentType === 'text/html' && d.querySelector('img') && d.querySelector('img').complete } catch (e) { return false } })()`)
    check('the SVG file is shown in an <img>', shown)
    await sleep(500)
    check('its script did not run', (await v.evaluate('document.title')) !== 'svg-script-ran', await v.evaluate('document.title'))
    await v.close()
  }

  // Revoking stops an open page: the worker keeps nothing.
  if (process.env.REDROP_CLI) {
    const v = await visitor()
    await v.send('Page.navigate', { url: links.probe.url })
    await v.waitFor(`${frameText}.includes('probe-4471')`)
    execFileSync(process.env.REDROP_CLI, ['revoke', links.probe.id], { stdio: 'ignore' })
    await v.evaluate(`document.getElementById('rd-frame').contentDocument.getElementById('rel').click()`)
    check('after revocation the open page gets nothing more', await v.waitFor(`${frameText}.includes("doesn't exist")`), await v.evaluate(frameText))
    await v.close()
  }
} catch (e) {
  check('the browser run', false, e && e.stack ? e.stack : String(e))
} finally {
  chrome.kill('SIGKILL')
  await sleep(300)
  rmSync(profile, { recursive: true, force: true })
}
process.exit(failed ? 1 : 0)
