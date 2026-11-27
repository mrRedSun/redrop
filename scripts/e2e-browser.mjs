#!/usr/bin/env node
// End-to-end encrypted links in a real browser (headless Chromium over the
// DevTools protocol, no packages), for scripts/e2e.sh. DESIGN.md
// "End-to-end encrypted links"; review 33 B-2, M1 and m-A, its re-check
// (B-2a, B-2b) and the second re-check (B-2a', B-2b', M3, m-1).
//
//   node scripts/e2e-browser.mjs links.json
//
// links.json holds {probe, password, once, svg}: {id, url} each (url is the
// full link), plus password.password. The probe is scripts/e2e-browser-probe.html
// shared as a folder with docs/two.html. Set REDROP_CLI (with REDROP_CONFIG)
// to also check that revoking stops an open page. Each check prints ok or
// FAIL; the exit code is 1 when any failed. Links stay out of the output.
// Set E2E_ELSEWHERE to a page on another origin (the API's /healthz) for the
// back/forward cache case.
// Plain-HTTP content origins are marked secure for this browser only
// (--unsafely-treat-insecure-origin-as-secure), since browsers give WebCrypto
// and service workers to secure contexts alone.
// The run is bounded: every DevTools call gives up after CALL_MS, the whole
// run after RUN_MS, and each second re-check case gets its own browser
// context, closed after it. scripts/e2e.sh also caps its memory.
import { spawn, execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const links = JSON.parse(readFileSync(process.argv[2], 'utf8'))
const keyOf = (l) => l.split('#k=')[1]
const secrets = Object.values(links).map((l) => keyOf(l.url)).filter(Boolean)
// hide masks keys in the output, and damaged ones (any run of key
// characters that starts like a key).
const hide = (s) => secrets.reduce((t, k) => t.replaceAll(k, '<KEY>').replace(new RegExp(k.slice(0, 12) + '[A-Za-z0-9_-]*', 'g'), '<KEY>'), String(s))
const origins = [...new Set(Object.values(links).map((l) => new URL(l.url).origin))]
  .filter((o) => o.startsWith('http:') && !/\/\/(localhost|[^/]*\.localhost)(:|$)/.test(o))
const profile = mkdtempSync(join(tmpdir(), 'redrop-e2e-chromium-'))
const args = ['--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--remote-debugging-port=0', `--user-data-dir=${profile}`]
if (origins.length) args.push(`--unsafely-treat-insecure-origin-as-secure=${origins.join(',')}`)
const chrome = spawn(process.env.CHROMIUM || 'chromium', [...args, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const CALL_MS = 20000
const RUN_MS = 10 * 60 * 1000
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
  // send gives up after CALL_MS, so a page that stops answering fails the
  // check that asked instead of hanging the run.
  const send = (method, params = {}) => new Promise((resolve) => {
    const i = ++id
    const timer = setTimeout(() => { if (pending.delete(i)) resolve({ error: { message: `${method} timed out` } }) }, CALL_MS)
    pending.set(i, (msg) => { clearTimeout(timer); resolve(msg) })
    ws.send(JSON.stringify({ id: i, method, params }))
  })
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
  const notCached = []
  // Every same-document navigation the browser made, by URL: a key in one
  // means the key entered a window the artifact shares.
  const sameDocument = []
  p.on((msg) => {
    if (msg.method === 'Page.navigatedWithinDocument') sameDocument.push(msg.params.url)
    if (msg.method === 'Network.requestWillBeSent') requests.push(msg.params.request.url + ' ' + (msg.params.request.headers.Referer ?? ''))
    if (msg.method === 'Page.backForwardCacheNotUsed') notCached.push(...msg.params.notRestoredExplanations.map((e) => e.reason))
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
  // open navigates the tab like the address bar and waits for the new
  // document (a mark on the old window is gone) to satisfy until.
  const open = async (url, until, ms) => {
    await evaluate('window.rdOld = 1')
    await p.send('Page.navigate', { url })
    return waitFor(`window.rdOld === undefined && (${until})`, ms)
  }
  return { ...p, evaluate, waitFor, open, requests, notCached, sameDocument, close: () => b.send('Target.disposeBrowserContext', { browserContextId: ctx }) }
}
const frameText = `(() => { try { return document.getElementById('rd-frame').contentDocument.body.innerText } catch (e) { return '' } })()`
const frameData = `(() => { try { return JSON.stringify(document.getElementById('rd-frame').contentDocument.body.dataset) } catch (e) { return '{}' } })()`
const pageText = `document.body ? document.body.innerText : ''`
const probeDone = `${frameText}.includes('probe-4471') && JSON.parse(${frameData}).done === '1'`
// What the probe artifact saw that looked like a key, from any document.
const stolen = `(() => { try { return localStorage.getItem('rd-probe-stolen') || '' } catch (e) { return 'ERR ' + e.name } })()`
// What the probe's timer in the viewer's window last copied from the
// viewer's address; it outlives the probe's own document.
const liveHref = `(() => { try { return localStorage.getItem('rd-probe-live-href') || '' } catch (e) { return 'ERR ' + e.name } })()`
const reset = `(() => { for (const k of ['rd-probe-stolen', 'rd-probe-live-href', 'rd-probe-acted', 'rd-probe-mode']) localStorage.removeItem(k); return true })()`
const guardShown = `(() => { const m = document.getElementById('rd-e2e-msg'); return !!m && !m.hidden && /new tab/.test(m.textContent) })()`

// The whole run is bounded too: past RUN_MS it fails and stops the browser.
const deadline = setTimeout(() => {
  check('the browser run finishes in time', false, `over ${RUN_MS / 60000} minutes`)
  chrome.kill('SIGKILL')
  rmSync(profile, { recursive: true, force: true })
  process.exit(1)
}, RUN_MS)
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
    check('the probe page decrypts in the frame', await v.waitFor(probeDone))
    const found = JSON.parse((await v.evaluate(frameData)) || '{}')
    for (const [route, value] of Object.entries(found)) {
      if (route === 'done') continue
      check(`a script in the artifact finds no key in ${route}`, !String(value).includes(key) && !String(value).includes('#k='), String(value))
    }
    check('the stored key is a non-extractable CryptoKey', /^\[\{"type":"secret","extractable":false,"algorithm":"HKDF"\}\]$/.test(found.storedKeys ?? ''), found.storedKeys)
    check('the artifact can\'t export the stored key', String(found.exportKey).startsWith('refused'), found.exportKey)
    const href = await v.evaluate('location.href')
    check('the address bar has no key and the live viewer\'s marker', !href.includes(key) && new URL(href).search === '?redrop-e2e=1' && new URL(href).hash === '#/', href)
    await v.evaluate(`document.getElementById('rd-frame').contentDocument.getElementById('rel').click()`)
    check('a relative link opens the next decrypted page', await v.waitFor(`${frameText}.includes('two-8842')`))
    await v.waitFor(`location.hash === '#/docs/two.html#sec'`, 5000)
    check('the address bar follows in the fragment', (await v.evaluate('location.hash')) === '#/docs/two.html#sec', await v.evaluate('location.href'))
    await v.send('Page.reload', {})
    check('a reload opens the same page with the stored key', await v.waitFor(`${frameText}.includes('two-8842')`))
    await v.send('Page.reload', { ignoreCache: true })
    check('a hard reload too', await v.waitFor(`${frameText}.includes('two-8842')`))

    // The full link again in the same tab (re-check B-2a), while the probe
    // runs in the frame: a new document, never a fragment change the open
    // artifact could watch.
    check('the full link opened in the same tab decrypts', await v.open(links.probe.url, probeDone))
    check('the open artifact saw no key before the reopen', (await v.evaluate(stolen)) === '', await v.evaluate(stolen))
    check('the full link reopened over the running probe decrypts', await v.open(links.probe.url, probeDone))
    await sleep(500)
    check('the open artifact saw no key in the same-tab reopen', (await v.evaluate(stolen)) === '', await v.evaluate(stolen))

    // Away and back: the viewer comes back from the back/forward cache with
    // the artifact running, then the full link is opened over it.
    if (process.env.E2E_ELSEWHERE) {
      await v.evaluate(`localStorage.removeItem('rd-probe-restored'); window.rdLive = 1`)
      await v.send('Page.navigate', { url: process.env.E2E_ELSEWHERE })
      await v.waitFor(`location.href.startsWith(${JSON.stringify(process.env.E2E_ELSEWHERE)})`)
      const history = (await v.send('Page.getNavigationHistory')).result
      v.notCached.length = 0
      await v.send('Page.navigateToHistoryEntry', { entryId: history.entries[history.currentIndex - 1].id })
      check('back restores the live viewer from the back/forward cache', await v.waitFor(`window.rdLive === 1 && !!localStorage.getItem('rd-probe-restored') && ${probeDone}`, 15000), v.notCached.join(', '))
      check('the full link opened over the restored viewer decrypts', await v.open(links.probe.url, probeDone))
      await sleep(500)
      check('the restored artifact saw no key', (await v.evaluate(stolen)) === '', await v.evaluate(stolen))
    } else {
      check('E2E_ELSEWHERE is set for the back/forward cache case', false)
    }

    // Damaged links after a good visit (re-check B-2b): any fragment that
    // starts with k= is a key attempt; a damaged one frames nothing, even
    // with the key stored, and leaves the address bar.
    const base = links.probe.url.split('#')[0]
    for (const [name, bad] of [['a trailing .', key + '.'], ['a trailing )', key + ')'], ['a trailing %20', key + '%20'], ['its last character cut', key.slice(0, -1)]]) {
      check(`the good link opens before the one with ${name}`, await v.open(links.probe.url, probeDone))
      const shown = await v.open(`${base}#k=${bad}`, `!document.getElementById('rd-e2e-msg').hidden`, 15000)
      const state = JSON.parse((await v.evaluate(`JSON.stringify({ msg: document.getElementById('rd-e2e-msg').textContent, src: document.getElementById('rd-frame').src, href: location.href })`)) || '{}')
      check(`a key with ${name}, after a good visit, says the link is damaged`, shown && /damaged/.test(state.msg ?? ''), state.msg)
      check(`  and frames nothing`, state.src === '', state.src)
      check(`  and leaves the address bar`, typeof state.href === 'string' && !state.href.includes('k=') && !state.href.includes(key.slice(0, 20)), state.href)
    }
    check('the artifact saw no key, whole or damaged, at any point', (await v.evaluate(stolen)) === '', await v.evaluate(stolen))
    check('a good link still opens after the damaged ones', await v.open(links.probe.url, probeDone))

    const leaked = v.requests.filter((r) => r.includes(key) || r.includes('#k=') || r.includes(key.slice(0, 20)))
    check('no request carried the key in its URL or Referer', leaked.length === 0, `${leaked.length} of ${v.requests.length}`)
    await v.close()
  }

  // The second re-check of review 33. Each case starts a fresh browser
  // context with a good visit (so the key is stored there), runs once, and
  // closes the context.
  {
    const key = keyOf(links.probe.url)
    const base = links.probe.url.split('#')[0]
    const noKey = (v, label) => {
      const sent = v.requests.filter((r) => r.includes(key.slice(0, 20)) || r.includes('#k='))
      check(`${label}: no request carried the key`, sent.length === 0, `${sent.length} of ${v.requests.length}`)
    }
    const notInWindow = (v, label) => {
      const urls = v.sameDocument.filter((u) => u.includes(key.slice(0, 20)))
      check(`${label}: the key never entered an open page by a same-document navigation`, urls.length === 0, urls.join(' '))
    }
    const goodVisit = async (label) => {
      const v = await visitor()
      await v.send('Page.navigate', { url: links.probe.url })
      check(`${label}: the good link opens first`, await v.waitFor(probeDone))
      return v
    }

    // Fragments that could hold a key but don't start with a literal k=
    // (B-2b'): damaged, after a good visit, framing nothing.
    for (const frag of [`k%3D${key}`, `k%253D${key}`, `%6B=${key}`, `K=${key}`, `&k=${key}`, `%20k=${key}`, `x&k=${key}`, key]) {
      const name = '#' + frag.replace(key, 'KEY')
      const v = await goodVisit(name)
      const shown = await v.open(`${base}#${frag}`, `!document.getElementById('rd-e2e-msg').hidden`, 15000)
      const state = JSON.parse((await v.evaluate(`JSON.stringify({ msg: document.getElementById('rd-e2e-msg').textContent, src: document.getElementById('rd-frame').src, href: location.href })`)) || '{}')
      check(`${name} after a good visit says the link is damaged and frames nothing`, shown && /damaged/.test(state.msg ?? '') && state.src === '', `${state.msg} ${state.src}`)
      check(`  and leaves the address bar`, typeof state.href === 'string' && !state.href.includes(key.slice(0, 20)), state.href)
      check(`  and the page saw no key`, (await v.evaluate(stolen)) === '', await v.evaluate(stolen))
      noKey(v, name)
      await v.close()
    }

    // A fragment typed after the live address (m-1): a same-document
    // navigation, which the viewer cancels before the URL changes, ending
    // the open page.
    {
      const v = await goodVisit('fragment edit')
      await v.send('Page.navigate', { url: `${base}?redrop-e2e=1#k=${key}` })
      check('a #k= typed after the live address closes the page and asks for a new tab', await v.waitFor(guardShown, 10000), await v.evaluate(pageText))
      check('  frames nothing and keeps the key off the address bar', (await v.evaluate(`document.querySelectorAll('iframe').length === 0 && !location.href.includes('k=')`)) === true, await v.evaluate('location.href'))
      check('  and the page saw no key', (await v.evaluate(stolen)) === '' && !(await v.evaluate(liveHref)).includes('k='), (await v.evaluate(stolen)) + (await v.evaluate(liveHref)))
      notInWindow(v, 'fragment edit')
      await v.close()
    }

    // An artifact that acts (B-2a'): it rewrites the viewer's address so the
    // full link would be a fragment change, also after replacing the
    // functions the viewer could use, or by replacing the viewer's document.
    // The full link is then opened in the tab. (document.open() also ends the
    // probe's own code, so there that case checks that the key never enters
    // the page, which the guard ensures; only the other two show a leak
    // without it.)
    for (const mode of ['late', 'hard', 'docopen']) {
      const v = await goodVisit(`[${mode}]`)
      await v.evaluate(`localStorage.setItem('rd-probe-mode', ${JSON.stringify(mode)})`)
      const acted = await v.open(links.probe.url, `localStorage.getItem('rd-probe-acted') === ${JSON.stringify(mode)}`, 20000)
      check(`[${mode}] the probe acted`, acted)
      if (mode !== 'docopen') {
        // Rewriting the address is itself a navigation the viewer didn't
        // start: refused, and the page closed.
        check(`[${mode}] the rewrite is refused and closes the page`, await v.waitFor(guardShown, 10000), await v.evaluate(pageText))
        check(`[${mode}]   the address keeps the live marker`, (await v.evaluate('location.search')) === '?redrop-e2e=1', await v.evaluate('location.href'))
      }
      await sleep(500)
      const before = await v.evaluate('location.href')
      await v.send('Page.navigate', { url: links.probe.url })
      await sleep(1500)
      const saw = (await v.evaluate(stolen)) + (await v.evaluate(liveHref))
      check(`[${mode}] the full link opened in its tab gives the page no key`, (await v.evaluate(stolen)) === '' && !(await v.evaluate(liveHref)).includes('k='), saw)
      check(`[${mode}]   and the address bar never holds it`, !(await v.evaluate('location.href')).includes('k='), `${before} -> ${await v.evaluate('location.href')}`)
      notInWindow(v, `[${mode}]`)
      await v.close()
    }

    // A link whose # became %23 (M3): the server's fixed page, no viewer, no
    // frame. Only that request carries the key, as any browser must send it.
    for (const enc of ['%23', '%2523']) {
      const v = await visitor()
      const shown = await v.open(`${base}${enc}k=${key}`, `(${pageText}).includes('This link is damaged')`, 15000)
      check(`a link whose # became ${enc} gets the damaged-link page and no frame`, shown && (await v.evaluate(`!document.getElementById('rd-frame')`)) === true, await v.evaluate(pageText))
      const other = v.requests.filter((r) => !r.startsWith(`${base}${enc}k=${key} `) && r.includes(key.slice(0, 20)))
      check(`  no other request carried the key`, other.length === 0, `${other.length} of ${v.requests.length}`)
      await v.close()
    }
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
  clearTimeout(deadline)
  chrome.kill('SIGKILL')
  await sleep(300)
  rmSync(profile, { recursive: true, force: true })
}
process.exit(failed ? 1 : 0)
