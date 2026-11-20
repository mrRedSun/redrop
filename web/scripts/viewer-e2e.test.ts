// Unit test for the end-to-end part of the viewer page script
// (internal/content/static/viewer.js with e2e.js, DESIGN.md "End-to-end
// encrypted links", "Keys and links" and "Viewing"). It runs the real
// sources in a VM against a fake viewer page, a fake service worker
// container and a fake server holding the Go vectors' manifest. The frame
// records every URL it is given, so the tests can show it never sees the key.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const read = (p: string) => readFileSync(new URL(p, import.meta.url), 'utf8')
const viewer = read('../../internal/content/static/viewer.js')
const lib = read('../../internal/content/static/e2e.js')
const vectors = JSON.parse(read('../../internal/e2e/testdata/vectors.json')) as {
  key: string; artifact: string; key_check: string; objects: Array<{ type: number; object: string }>
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

function page(opts: { start: string; stored?: string; secure?: boolean; manifest?: (b: Uint8Array) => Uint8Array; version?: string } = { start: '/' }) {
  const top = new URL(opts.start, ORIGIN)
  const location = { origin: ORIGIN, pathname: top.pathname, search: top.search, hash: top.hash }
  const version = opts.version ?? '3'
  const frame = new El({
    'data-e2e': '', 'data-src': `/__redrop/v/${version}${top.pathname}${top.search}`, 'data-artifact': vectors.artifact,
    'data-version': version, 'data-current': '3', 'data-key-check': vectors.key_check,
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
  const session = new Map<string, string>(opts.stored ? [['redrop-e2e-key', opts.stored]] : [])
  const posted: Array<{ type: string; key: unknown; artifact: string }> = []
  const registered: unknown[] = []
  const swListeners: Record<string, Listener[]> = {}
  const activeWorker = { state: 'activated', postMessage: (m: { type: string; key: unknown; artifact: string }) => posted.push(m) }
  const fetched: string[] = []
  const manifest = vectors.objects.find((o) => o.type === 1)!
  const context = vm.createContext({
    document: doc, location, URL, URLSearchParams, Date, Intl, Array, Uint8Array,
    isSecureContext: opts.secure ?? true,
    crypto: globalThis.crypto, CryptoKey: globalThis.CryptoKey, TextEncoder, TextDecoder, atob, btoa, ReadableStream,
    setInterval: () => 0, clearInterval() {}, setTimeout,
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
    sessionStorage: { getItem: (k: string) => session.get(k) ?? null, setItem: (k: string, v: string) => void session.set(k, v) },
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
      if (url !== `/__redrop/v/${version}/manifest`) return new Response('Not found', { status: 404 })
      const bytes = Uint8Array.from(Buffer.from(manifest.object, 'base64'))
      return new Response((opts.manifest ? opts.manifest(bytes) : bytes) as Uint8Array<ArrayBuffer>, { status: 200 })
    },
  })
  const reloads: number[] = []
  const windowListeners: Record<string, Listener[]> = {}
  Object.assign(context, {
    window: context,
    addEventListener(type: string, fn: Listener) { (windowListeners[type] ??= []).push(fn) },
  })
  ;(location as Record<string, unknown>).reload = () => reloads.push(1)
  vm.runInContext(lib, context)
  vm.runInContext(viewer, context)
  return {
    frame, note, doc, replaced, session, posted, registered, fetched, reloads,
    hashchange() { for (const fn of windowListeners.hashchange ?? []) fn() },
    // settled waits for the boot to frame the page or show a message.
    async settled() {
      for (let i = 0; i < 400 && frame.srcs.length === 0 && note.hidden; i++) await new Promise((r) => setTimeout(r, 5))
    },
    load(url: string) {
      frameWin = { location: new URL(url, ORIGIN) }
      frame.dispatch('load')
    },
    workerAsks() {
      const answered: unknown[] = []
      for (const fn of swListeners.message ?? []) fn({ data: { type: 'redrop-e2e-need-key' }, source: { postMessage: (m: unknown) => answered.push(m) } })
      return answered
    },
  }
}

const anyKey = (s: string) => s.includes(vectors.key) || s.includes('#k=')

await test('the key leaves the address bar, stays in the tab, and only the worker gets it', async () => {
  const p = page({ start: `/#k=${vectors.key}` })
  await p.settled()
  assert.equal(p.note.hidden, true, p.note.textContent)
  assert.deepEqual(p.replaced, ['/'])
  assert.equal(p.session.get('redrop-e2e-key'), vectors.key)
  assert.deepEqual(p.frame.srcs, ['/__redrop/v/3/'])
  assert.ok(!p.frame.srcs.some(anyKey), 'the frame never sees the key')
  assert.equal(p.posted.length, 1)
  assert.equal(p.posted[0].type, 'redrop-e2e-key')
  assert.equal(p.posted[0].artifact, vectors.artifact)
  assert.ok(p.posted[0].key instanceof CryptoKey && !(p.posted[0].key as CryptoKey).extractable, 'a non-extractable CryptoKey, never the text')
  assert.deepEqual(JSON.parse(JSON.stringify(p.registered)), [['/__redrop/e2e-sw.js', { scope: '/__redrop/v/', updateViaCache: 'none' }]])
  assert.equal(p.doc.title, 'Vector', 'the tab shows the decrypted title')
  // A worker that was stopped asks again and gets the same key object kind.
  const answered = p.workerAsks() as Array<{ type: string; key: unknown }>
  assert.equal(answered.length, 1)
  assert.ok(answered[0].key instanceof CryptoKey)
})

await test('a page in the fragment opens in the frame; the artifact\'s own fragment is handed on', async () => {
  const deep = page({ start: `/#k=${vectors.key}&/docs/a.html?x=1#part` })
  await deep.settled()
  assert.deepEqual(deep.frame.srcs, ['/__redrop/v/3/docs/a.html?x=1#part'])
  assert.deepEqual(deep.replaced, ['/#/docs/a.html?x=1#part'])
  const own = page({ start: `/report.html#k=${vectors.key}&totals` })
  await own.settled()
  assert.deepEqual(own.frame.srcs, ['/__redrop/v/3/report.html#totals'])
  assert.deepEqual(own.replaced, ['/report.html#totals'])
  for (const p of [deep, own]) {
    assert.ok(!p.frame.srcs.some(anyKey) && !p.replaced.some(anyKey))
  }
})

await test('a reload finds the key in the tab', async () => {
  const p = page({ start: '/#/docs/a.html', stored: vectors.key })
  await p.settled()
  assert.deepEqual(p.frame.srcs, ['/__redrop/v/3/docs/a.html'])
  assert.deepEqual(p.replaced, [], 'nothing to remove')
})

await test('the address bar keeps the frame\'s page in the fragment, so the server sees only "/"', async () => {
  const p = page({ start: `/#k=${vectors.key}` })
  await p.settled()
  p.load('/__redrop/v/3/docs/a.html?x=1#part')
  p.load('/__redrop/v/2/old.html')
  assert.deepEqual(p.replaced.slice(1), ['/#/docs/a.html?x=1#part', '/?redrop-version=2#/old.html'])
})

await test('without a key, with another key, or with damaged content, nothing is framed', async () => {
  const missing = page({ start: '/' })
  await missing.settled()
  assert.match(missing.note.textContent, /key is missing/)
  const wrong = page({ start: `/#k=${OTHER_KEY}` })
  await wrong.settled()
  assert.match(wrong.note.textContent, /key doesn't open this page/)
  const damaged = page({ start: `/#k=${vectors.key}`, manifest: (b) => { const o = b.slice(); o[o.length - 1] ^= 1; return o } })
  await damaged.settled()
  assert.match(damaged.note.textContent, /integrity check/)
  const malformed = page({ start: `/#k=${vectors.key.slice(0, 42)}B` })
  await malformed.settled()
  assert.match(malformed.note.textContent, /damaged/)
  const insecure = page({ start: `/#k=${vectors.key}`, secure: false })
  await insecure.settled()
  assert.match(insecure.note.textContent, /HTTPS/)
  for (const p of [missing, wrong, damaged, malformed, insecure]) {
    assert.equal(p.note.hidden, false)
    assert.deepEqual(p.frame.srcs, [])
    assert.deepEqual(p.registered, [], 'no worker without a working key')
    assert.equal(p.posted.length, 0)
  }
  assert.deepEqual(wrong.fetched, [], 'a wrong key is caught by the key check, before any fetch')
})

await test('a full link pasted into an open viewer starts over with its key', async () => {
  const p = page({ start: '/' })
  await p.settled()
  assert.match(p.note.textContent, /key is missing/)
  p.hashchange()
  assert.equal(p.reloads.length, 1)
})
