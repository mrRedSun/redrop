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
  removed = false
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

// The Navigation API as the guard uses it, per page, so a test can tamper
// with one page's prototypes the way an artifact could.
function navigationAPI() {
  class PageEvent extends Event {
    get cancelable() { return super.cancelable }
    stopImmediatePropagation() { super.stopImmediatePropagation() }
    preventDefault() { super.preventDefault() }
  }
  class NavigationDestination {
    constructor(private readonly u: string, private readonly same: boolean) {}
    get url() { return this.u }
    get sameDocument() { return this.same }
  }
  class NavigateEvent extends PageEvent {
    constructor(private readonly dest: NavigationDestination, cancelable: boolean) { super('navigate', { cancelable }) }
    get destination() { return this.dest }
  }
  return { PageEvent, NavigationDestination, NavigateEvent, navigation: new EventTarget() }
}

function page(browser: Browser, opts: { start: string; secure?: boolean; noNavigation?: boolean; manifest?: (b: Uint8Array) => Uint8Array }) {
  const top = new URL(opts.start, ORIGIN)
  const location = { origin: ORIGIN, pathname: top.pathname, search: top.search, hash: top.hash }
  const api = navigationAPI()
  // navigate fires the navigate event for a same-document change to url,
  // like the browser, and applies it unless a listener cancelled it.
  const navigate = (url: string) => {
    const u = new URL(url, ORIGIN)
    const go = api.navigation.dispatchEvent(new api.NavigateEvent(new api.NavigationDestination(u.href, true), true))
    if (go) Object.assign(location, { pathname: u.pathname, search: u.search, hash: u.hash })
    return go
  }
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
  Object.assign(doc, { querySelectorAll: (sel: string) => (sel.includes('iframe') && !frame.removed ? [frame] : []) })
  const replaced: string[] = []
  const reloads: number[] = []
  const posted: Array<{ type: string; key: unknown; artifact: string }> = []
  const registered: unknown[] = []
  const swListeners: Record<string, Listener[]> = {}
  const windowListeners: Record<string, Listener[]> = {}
  const activeWorker = { state: 'activated', postMessage: (m: { type: string; key: unknown; artifact: string }) => posted.push(m) }
  const fetched: string[] = []
  class History {
    get state() { return null }
    replaceState(_s: unknown, _t: string, url: string) {
      if (navigate(url)) replaced.push(url)
    }
    pushState() { throw new Error('the viewer must never push history entries') }
  }
  const manifest = vectors.objects.find((o) => o.type === 1 && o.version === 3)!
  const context = vm.createContext({
    document: doc, location, URL, URLSearchParams, Date, Intl, Array, Uint8Array,
    isSecureContext: opts.secure ?? true,
    crypto: globalThis.crypto, CryptoKey: globalThis.CryptoKey, TextEncoder, TextDecoder, atob, btoa, ReadableStream,
    setInterval: () => 0, clearInterval() {}, setTimeout,
    addEventListener(type: string, fn: Listener) { (windowListeners[type] ??= []).push(fn) },
    history: new History(), History, EventTarget,
    Event: api.PageEvent, NavigateEvent: api.NavigateEvent, NavigationDestination: api.NavigationDestination,
    ...(opts.noNavigation ? {} : { navigation: api.navigation }),
    Document: { prototype: { querySelectorAll(this: { querySelectorAll(s: string): unknown }, sel: string) { return this.querySelectorAll(sel) } } },
    Element: { prototype: { remove(this: El) { this.removed = true } } },
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
    frame, note, doc, replaced, reloads, posted, registered, fetched, context, api,
    get href() { return location.pathname + location.search + location.hash },
    // visit opens url in this tab, like the address bar: a new document
    // unless only the fragment changes, which is this page's navigation.
    visit(url: string) {
      const u = new URL(url, ORIGIN)
      if (u.pathname !== location.pathname || u.search !== location.search) return 'new document'
      if (!navigate(url)) return 'cancelled'
      for (const fn of windowListeners.hashchange ?? []) fn()
      return 'changed'
    },
    // watch adds a navigate listener the way the artifact would, after the
    // frame exists, and returns every URL it saw.
    watch() {
      const seen: string[] = []
      api.navigation.addEventListener('navigate', (e) => seen.push((e as InstanceType<typeof api.NavigateEvent>).destination.url))
      return seen
    },
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

// The second re-check of review 33, B-2b': anything that could hold a key
// is a key attempt, encoded or in another case, wherever it sits in the
// fragment. With the key stored, none of it reaches the frame.
await test('encoded, re-cased or displaced key fragments after a good visit open nothing', async () => {
  const browser = newBrowser()
  await open(browser, `/#k=${vectors.key}`)
  const K = vectors.key
  for (const frag of [`k%3D${K}`, `k%3d${K}`, `k%253D${K}`, `%6B=${K}`, `K=${K}`, `&k=${K}`, `%20k=${K}`, `x&k=${K}`, `/docs/a.html?k=${K}`, K]) {
    const p = page(browser, { start: `/#${frag}` })
    await p.settled()
    assert.match(p.note.textContent, /damaged/, frag)
    assert.deepEqual(p.frame.srcs, [], frag)
    assert.deepEqual(p.fetched, [], frag)
    assert.ok(!p.href.includes(K.slice(0, 20)), `${frag}: ${p.href}`)
  }
})

// M3: a link whose # became %23 in the path.
await test('a path holding an encoded # opens nothing and leaves the address bar', async () => {
  const browser = newBrowser()
  await open(browser, `/#k=${vectors.key}`)
  for (const path of [`/%23k=${vectors.key}`, `/%2523k=${vectors.key}`]) {
    const p = page(browser, { start: path })
    await p.settled()
    assert.match(p.note.textContent, /changed to %23/, path)
    assert.deepEqual(p.frame.srcs, [], path)
    assert.equal(p.href, '/?redrop-e2e=1')
  }
})

// B-2a': once the frame exists, no address change the viewer didn't make
// goes through: not the full link opened in this tab (whatever the address
// is then), not a fragment typed after the live address, and not the
// artifact rewriting the address to set that up. Each one ends the page.
await test('a link opened in the tab of an open page is refused, before anything sees it, and the page ends', async () => {
  for (const reopen of [`/?redrop-e2e=1#k=${vectors.key}`, '/?redrop-e2e=1#/docs/a.html']) {
    const browser = newBrowser()
    const { second } = await open(browser, `/#k=${vectors.key}`)
    const seen = second.watch()
    const before = second.href
    assert.equal(second.visit(reopen), 'cancelled', reopen)
    assert.deepEqual(seen, [], 'a later listener never sees the URL')
    assert.equal(second.href, before, 'the address never changed')
    assert.equal(second.frame.removed, true, 'the frame is gone')
    assert.match(second.note.textContent, /new tab/)
    assert.deepEqual(second.reloads, [])
  }
})

await test('the artifact rewriting the address ends it, and the full link then loads a new page', async () => {
  const { second } = await open(newBrowser(), `/#k=${vectors.key}`)
  vm.runInContext(`history.replaceState(null, '', '/#/')`, second.context)
  assert.equal(second.href, '/?redrop-e2e=1', 'the marker stays')
  assert.equal(second.frame.removed, true)
  assert.match(second.note.textContent, /new tab/)
  assert.equal(second.visit(`/#k=${vectors.key}`), 'new document')
})

await test('an artifact that replaces the functions the guard uses doesn\'t disable it', async () => {
  const { second } = await open(newBrowser(), `/#k=${vectors.key}`)
  vm.runInContext(`
    Event.prototype.preventDefault = function () {}
    Event.prototype.stopImmediatePropagation = function () {}
    Object.defineProperty(NavigateEvent.prototype, 'destination', { get: function () { throw new Error('no') } })
    Object.defineProperty(NavigationDestination.prototype, 'sameDocument', { get: function () { return false } })
    History.prototype.replaceState = function () {}
    Element.prototype.remove = function () {}
    Document.prototype.querySelectorAll = function () { return [] }
    Reflect.apply = function () {}
  `, second.context)
  const seen = second.watch()
  assert.equal(second.visit(`/?redrop-e2e=1#k=${vectors.key}`), 'cancelled')
  assert.deepEqual(seen, [])
  assert.equal(second.frame.removed, true)
})

await test('without the Navigation API nothing is opened, and the key leaves the address bar', async () => {
  const first = page(newBrowser(), { start: `/#k=${vectors.key}`, noNavigation: true })
  await first.settled()
  assert.match(first.note.textContent, /Navigation API/)
  assert.deepEqual(first.reloads, [])
  assert.ok(!anyKey(first.href))
  const browser = newBrowser()
  await open(browser, `/#k=${vectors.key}`)
  const later = page(browser, { start: '/?redrop-e2e=1', noNavigation: true })
  await later.settled()
  assert.match(later.note.textContent, /Navigation API/)
  assert.deepEqual(later.frame.srcs, [], 'not even with the key stored')
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
