// Unit test for the end-to-end part of the viewer page script
// (internal/content/static/viewer.js with e2e.js, DESIGN.md "End-to-end
// encrypted links", "Keys and links" and "Viewing"). It runs the real
// sources in a VM against a fake viewer page, a fake service worker
// container, a fake key store and a fake server holding the Go vectors'
// manifest. A "browser" outlives page loads, so a test follows the viewer
// through its own reload. The frame records every URL it is given, so the
// tests can show it never sees the key, and the key never sits in
// sessionStorage or the address bar.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const read = (p: string) => readFileSync(new URL(p, import.meta.url), 'utf8')
const viewer = read('../../internal/content/static/viewer.js')
const lib = read('../../internal/content/static/e2e.js')
const vectors = JSON.parse(read('../../internal/e2e/testdata/vectors.json')) as {
  key: string; artifact: string; key_check: string; objects: Array<{ type: number; version: number; object: string }>
}
const ORIGIN = 'https://amber-otter-quilt-river.usercontent.example'
const OTHER_KEY = 'A'.repeat(43)

type Listener = (e?: unknown) => void

class El {
  listeners: Record<string, Listener[]> = {}
  attrs: Record<string, string> = {}
  hidden = true
  textContent = ''
  title = ''
  srcs: string[] = []
  constructor(attrs: Record<string, string> = {}) { this.attrs = attrs }
  getAttribute(k: string) { return this.attrs[k] ?? null }
  addEventListener(type: string, fn: Listener) { (this.listeners[type] ??= []).push(fn) }
  dispatch(type: string, e?: unknown) { for (const fn of this.listeners[type] ?? []) fn(e) }
  set src(v: string) { this.srcs.push(v) }
  get src() { return this.srcs[this.srcs.length - 1] ?? '' }
}

// What a browser keeps between the viewer's page loads on one origin.
type Browser = {
  keys: Map<string, CryptoKey>
  session: Map<string, string> // must stay empty: the key text is never kept
  storageFails?: boolean
}
const newBrowser = (): Browser => ({ keys: new Map(), session: new Map() })

function page(browser: Browser, opts: { start: string; secure?: boolean; manifest?: (b: Uint8Array) => Uint8Array }) {
  const top = new URL(opts.start, ORIGIN)
  const location = { origin: ORIGIN, pathname: top.pathname, search: top.search, hash: top.hash }
  // The server's frame address never holds the live viewer's marker.
  const own = new URLSearchParams(top.search)
  own.delete('redrop-e2e')
  const frame = new El({
    'data-e2e': '', 'data-src': `/__redrop/v/3${top.pathname}${own.size ? '?' + own : ''}`, 'data-artifact': vectors.artifact,
    'data-version': '3', 'data-current': '3', 'data-key-check': vectors.key_check,
  })
  let frameWin: { location: URL } | null = null
  Object.defineProperty(frame, 'contentWindow', {
    get: () => ({
      get location() { return frameWin!.location },
      get navigation() { return undefined },
      addEventListener() {}, removeEventListener() {},
    }),
  })
  const note = new El()
  const doc = Object.assign(new El(), {
    title: '',
    visibilityState: 'visible',
    querySelectorAll: () => [],
    getElementById: (id: string) => ({ 'rd-frame': frame, 'rd-e2e-msg': note } as Record<string, unknown>)[id] ?? null,
  })
  const replaced: string[] = []
  const reloads: number[] = []
  const posted: Array<{ type: string; key: unknown; artifact: string }> = []
  const registered: unknown[] = []
  const swListeners: Record<string, Listener[]> = {}
  const windowListeners: Record<string, Listener[]> = {}
  const activeWorker = { state: 'activated', postMessage: (m: { type: string; key: unknown; artifact: string }) => posted.push(m) }
  const fetched: string[] = []
  const manifest = vectors.objects.find((o) => o.type === 1 && o.version === 3)!
  const context = vm.createContext({
    document: doc, location, URL, URLSearchParams, Date, Intl, Array, Uint8Array,
    isSecureContext: opts.secure ?? true,
    crypto: globalThis.crypto, CryptoKey: globalThis.CryptoKey, TextEncoder, TextDecoder, atob, btoa, ReadableStream,
    setInterval: () => 0, clearInterval() {}, setTimeout,
    addEventListener(type: string, fn: Listener) { (windowListeners[type] ??= []).push(fn) },
    history: {
      state: null,
      replaceState(_s: unknown, _t: string, url: string) {
        replaced.push(url)
        const u = new URL(url, ORIGIN)
        location.pathname = u.pathname
        location.search = u.search
        location.hash = u.hash
      },
      pushState() { throw new Error('the viewer must never push history entries') },
    },
    sessionStorage: {
      getItem: (k: string) => browser.session.get(k) ?? null,
      setItem: (k: string, v: string) => void browser.session.set(k, v),
    },
    navigator: {
      serviceWorker: {
        addEventListener(type: string, fn: Listener) { (swListeners[type] ??= []).push(fn) },
        startMessages() {},
        register(url: string, o: unknown) {
          registered.push([url, o])
          return Promise.resolve({ installing: null, waiting: null, active: activeWorker })
        },
      },
    },
    fetch: async (url: string) => {
      fetched.push(url)
      if (url !== '/__redrop/v/3/manifest') return new Response('Not found', { status: 404 })
      const bytes = Uint8Array.from(Buffer.from(manifest.object, 'base64'))
      return new Response((opts.manifest ? opts.manifest(bytes) : bytes) as Uint8Array<ArrayBuffer>, { status: 200 })
    },
  })
  Object.assign(location, { reload: () => reloads.push(1) })
  ;(context as Record<string, unknown>).window = context
  vm.runInContext(lib, context)
  // The browser's IndexedDB, as the viewer sees it through RedropE2E.keys
  // (the real one is tested in e2e-crypto.test.ts).
  const E = (context as Record<string, unknown>).RedropE2E as Record<string, unknown>
  E.keys = {
    put: async (artifact: string, key: CryptoKey) => {
      if (browser.storageFails) throw Object.assign(new Error('storage'), { code: 'storage' })
      browser.keys.set(artifact, key)
    },
    get: async (artifact: string) => browser.keys.get(artifact) ?? null,
    remove: async (artifact: string) => void browser.keys.delete(artifact),
  }
  vm.runInContext(viewer, context)
  return {
    frame, note, doc, replaced, reloads, posted, registered, fetched,
    get href() { return location.pathname + location.search + location.hash },
    // settled waits for the boot to reload, frame the page, or show a
    // message. The bound only catches a hang.
    async settled() {
      for (let i = 0; i < 6000 && frame.srcs.length === 0 && note.hidden && reloads.length === 0; i++) await new Promise((r) => setTimeout(r, 5))
    },
    load(url: string) {
      frameWin = { location: new URL(url, ORIGIN) }
      frame.dispatch('load')
    },
    hashchange() { for (const fn of windowListeners.hashchange ?? []) fn() },
    workerAsks() {
      const answered: unknown[] = []
      for (const fn of swListeners.message ?? []) fn({ data: { type: 'redrop-e2e-need-key' }, source: { postMessage: (m: unknown) => answered.push(m) } })
      return answered
    },
  }
}

const anyKey = (s: string) => s.includes(vectors.key) || s.includes('#k=')

// open follows a link the way a visitor's browser does: the first load
// keeps the key and reloads; the reloaded page frames the artifact.
async function open(browser: Browser, link: string) {
  const first = page(browser, { start: link })
  await first.settled()
  assert.equal(first.reloads.length, 1, first.note.textContent)
  const second = page(browser, { start: first.href })
  await second.settled()
  return { first, second }
}

await test('the key becomes a non-extractable CryptoKey and leaves the address bar before anything is framed', async () => {
  const browser = newBrowser()
  const { first, second } = await open(browser, `/#k=${vectors.key}`)
  // First load: nothing framed, nothing registered, nothing fetched.
  assert.deepEqual(first.replaced, ['/?redrop-e2e=1'])
  assert.deepEqual(first.frame.srcs, [])
  assert.deepEqual(first.registered, [])
  assert.deepEqual(first.fetched, [])
  const stored = browser.keys.get(vectors.artifact)
  assert.ok(stored instanceof CryptoKey && !stored.extractable, 'kept as a non-extractable CryptoKey')
  assert.equal(browser.session.size, 0, 'the key text is never kept')
  // The reloaded page: the frame, the worker and the key, never its text.
  assert.equal(second.note.hidden, true, second.note.textContent)
  assert.deepEqual(second.replaced, [])
  assert.deepEqual(second.frame.srcs, ['/__redrop/v/3/'])
  assert.equal(second.posted.length, 1)
  assert.equal(second.posted[0].key, stored)
  assert.equal(second.posted[0].artifact, vectors.artifact)
  assert.deepEqual(JSON.parse(JSON.stringify(second.registered)), [['/__redrop/e2e-sw.js', { scope: '/__redrop/v/', updateViaCache: 'none' }]])
  assert.equal(second.doc.title, 'Vector', 'the tab shows the decrypted title')
  for (const p of [first, second]) {
    assert.ok(!p.frame.srcs.some(anyKey) && !anyKey(p.href), 'the frame and the address bar never hold the key')
  }
  // A worker that was stopped asks again and gets the CryptoKey.
  const answered = second.workerAsks() as Array<{ type: string; key: unknown }>
  assert.equal(answered.length, 1)
  assert.equal(answered[0].key, stored)
})

await test('a page in the fragment opens in the frame; the artifact\'s own fragment is handed on', async () => {
  const deep = await open(newBrowser(), `/#k=${vectors.key}&/docs/a.html?x=1#part`)
  assert.deepEqual(deep.first.replaced, ['/?redrop-e2e=1#/docs/a.html?x=1#part'])
  assert.deepEqual(deep.second.frame.srcs, ['/__redrop/v/3/docs/a.html?x=1#part'])
  const own = await open(newBrowser(), `/report.html#k=${vectors.key}&totals`)
  assert.deepEqual(own.first.replaced, ['/report.html?redrop-e2e=1#totals'])
  assert.deepEqual(own.second.frame.srcs, ['/__redrop/v/3/report.html#totals'])
  const query = await open(newBrowser(), `/report.html?q=1#k=${vectors.key}`)
  assert.deepEqual(query.first.replaced, ['/report.html?q=1&redrop-e2e=1'])
  assert.deepEqual(query.second.frame.srcs, ['/__redrop/v/3/report.html?q=1'])
})

await test('the address bar keeps the frame\'s page in the fragment, so the server sees only "/"', async () => {
  const { second } = await open(newBrowser(), `/#k=${vectors.key}`)
  second.load('/__redrop/v/3/docs/a.html?x=1#part')
  second.load('/__redrop/v/2/old.html')
  assert.deepEqual(second.replaced, ['/?redrop-e2e=1#/docs/a.html?x=1#part', '/?redrop-e2e=1&redrop-version=2#/old.html'])
})

// The re-check of review 33, B-2a: the live viewer's address always has the
// marker a full link never has, so opening the full link in the same tab is
// a new document, never a fragment change the open artifact could watch.
await test('the live viewer is always at an address a full link reaches only by loading a new page', async () => {
  const browser = newBrowser()
  const { second } = await open(browser, `/#k=${vectors.key}`)
  assert.equal(second.href, '/?redrop-e2e=1')
  for (const start of ['/', '/#/docs/a.html', '/?redrop-version=2#/old.html']) {
    const p = page(browser, { start })
    await p.settled()
    assert.equal(p.note.hidden, true, p.note.textContent)
    assert.equal(p.replaced.length, 1)
    assert.match(p.href, /^\/\?(.*&)?redrop-e2e=1(&|#|$)/, start)
    assert.ok(!p.frame.srcs.some((s) => s.includes('redrop-e2e')), 'the marker is never the artifact\'s')
  }
  second.load('/__redrop/v/3/docs/a.html')
  assert.ok(second.href.startsWith('/?redrop-e2e=1#'))
})

await test('without a key, with another key, damaged content or no storage, nothing is framed', async () => {
  const missing = page(newBrowser(), { start: '/' })
  await missing.settled()
  assert.match(missing.note.textContent, /key is missing/)
  const wrong = page(newBrowser(), { start: `/#k=${OTHER_KEY}` })
  await wrong.settled()
  assert.match(wrong.note.textContent, /key doesn't open this page/)
  const malformed = page(newBrowser(), { start: `/#k=${vectors.key.slice(0, 42)}B` })
  await malformed.settled()
  assert.match(malformed.note.textContent, /damaged/)
  const insecure = page(newBrowser(), { start: `/#k=${vectors.key}`, secure: false })
  await insecure.settled()
  assert.match(insecure.note.textContent, /HTTPS/)
  const blocked = { ...newBrowser(), storageFails: true }
  const noStorage = page(blocked, { start: `/#k=${vectors.key}` })
  await noStorage.settled()
  assert.match(noStorage.note.textContent, /keep the key/)
  const damagedBrowser = newBrowser()
  await page(damagedBrowser, { start: `/#k=${vectors.key}` }).settled()
  const damaged = page(damagedBrowser, { start: '/', manifest: (b) => { const o = b.slice(); o[o.length - 1] ^= 1; return o } })
  await damaged.settled()
  assert.match(damaged.note.textContent, /integrity check/)
  for (const p of [missing, wrong, malformed, insecure, noStorage, damaged]) {
    assert.equal(p.note.hidden, false)
    assert.deepEqual(p.frame.srcs, [])
    assert.deepEqual(p.registered, [], 'no worker without a working key')
    assert.deepEqual(p.reloads, [])
    assert.ok(!anyKey(p.href), 'the key left the address bar anyway')
  }
  for (const p of [wrong, malformed, insecure, noStorage]) assert.deepEqual(p.fetched, [], 'caught before any fetch')
})

// The re-check of review 33, B-2b: a fragment that starts with k= is a key
// attempt. A damaged one, even with a good key stored from an earlier
// visit, frames nothing, hands no part of itself on, and leaves the
// address bar.
await test('a damaged key after a good visit opens nothing and leaves the address bar', async () => {
  const browser = newBrowser()
  await open(browser, `/#k=${vectors.key}`)
  assert.equal(browser.keys.size, 1)
  for (const frag of [`k=${vectors.key}.`, `k=${vectors.key})`, `k=${vectors.key}%20`, `k=${vectors.key.slice(0, 42)}`, `k=${vectors.key}x&/docs/a.html`]) {
    const p = page(browser, { start: `/#${frag}` })
    await p.settled()
    assert.match(p.note.textContent, /damaged/, frag)
    assert.equal(p.note.hidden, false)
    assert.deepEqual(p.frame.srcs, [], frag)
    assert.deepEqual(p.registered, [], frag)
    assert.deepEqual(p.fetched, [], frag)
    assert.deepEqual(p.reloads, [], frag)
    assert.deepEqual(p.replaced, ['/?redrop-e2e=1'], frag)
    assert.ok(!p.href.includes('k='), frag)
  }
})

await test('a stored key that no longer matches is forgotten', async () => {
  const browser = newBrowser()
  const other = await crypto.subtle.importKey('raw', new Uint8Array(32), 'HKDF', false, ['deriveBits', 'deriveKey'])
  browser.keys.set(vectors.artifact, other)
  const p = page(browser, { start: '/' })
  await p.settled()
  assert.match(p.note.textContent, /key doesn't open this page/)
  assert.equal(browser.keys.size, 0)
})

await test('a full link pasted into an open viewer starts over with its key', async () => {
  const p = page(newBrowser(), { start: '/' })
  await p.settled()
  assert.match(p.note.textContent, /key is missing/)
  p.hashchange()
  assert.equal(p.reloads.length, 1)
})
