// Unit test for the end-to-end key intake (DESIGN.md "End-to-end encrypted
// links", "Where the key goes in the browser"): the app origin's intake
// page script (internal/content/static/e2e-intake.js) and the helper it
// opens on the artifact's origin (e2e-helper.js), each run with the real
// decryptor in a VM against fake pages, windows and messages. They check
// that the key's text leaves the address bar first, crosses no origin
// (only a non-extractable CryptoKey does), and that each side takes
// messages only from the other.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const read = (p: string) => readFileSync(new URL(p, import.meta.url), 'utf8')
const lib = read('../../internal/content/static/e2e.js')
const intake = read('../../internal/content/static/e2e-intake.js')
const helper = read('../../internal/content/static/e2e-helper.js')
const vectors = JSON.parse(read('../../internal/e2e/testdata/vectors.json')) as { key: string }
const APP = 'https://redrop.example.com'
const CONTENT = 'https://amber-otter-quilt-river.usercontent.example'
const KEY = vectors.key

type Listener = (e: unknown) => void
// plain copies a value made in the VM's realm, for deep comparison.
const plain = (v: unknown) => JSON.parse(JSON.stringify(v))

class El {
  hidden = true
  disabled = false
  textContent = ''
  listeners: Record<string, Listener[]> = {}
  attrs: Record<string, string>
  constructor(attrs: Record<string, string> = {}) { this.attrs = attrs }
  getAttribute(k: string) { return this.attrs[k] ?? null }
  addEventListener(type: string, fn: Listener) { (this.listeners[type] ??= []).push(fn) }
  click() { for (const fn of this.listeners.click ?? []) fn({}) }
}

// everything records every value the script hands to the outside, so a test
// can show the key's text is in none of them.
function intakePage(fragment: string, opts: { blocked?: boolean } = {}) {
  const calls: Array<[string, unknown[]]> = []
  const everything = () => JSON.stringify(calls, (_k, v) => (v instanceof CryptoKey ? '[CryptoKey]' : v))
  const els = {
    'rd-intake': new El({ 'data-content': CONTENT + '/' }),
    'rd-intake-msg': new El(), 'rd-intake-open': new El(), 'rd-intake-note': new El(),
  }
  const listeners: Listener[] = []
  const popup = { postMessage: (m: unknown, origin: string) => calls.push(['popup.postMessage', [m, origin]]) }
  const location = { pathname: '/e/amber-otter-quilt-river', hash: '#' + fragment, replace: (u: string) => calls.push(['location.replace', [u]]) }
  const context = vm.createContext({
    document: { getElementById: (id: keyof typeof els) => els[id] ?? null },
    location, URL, isSecureContext: true, crypto: globalThis.crypto, CryptoKey: globalThis.CryptoKey,
    TextEncoder, TextDecoder, atob, btoa, ReadableStream, setTimeout, clearTimeout,
    history: { replaceState: (_s: unknown, _t: string, u: string) => { calls.push(['history.replaceState', [u]]); location.hash = '' } },
    addEventListener: (type: string, fn: Listener) => { if (type === 'message') listeners.push(fn) },
    open: (...a: unknown[]) => { calls.push(['window.open', a]); return opts.blocked ? null : popup },
  })
  ;(context as Record<string, unknown>).window = context
  vm.runInContext(lib, context)
  vm.runInContext(intake, context)
  const message = (data: unknown, origin = CONTENT, source: unknown = popup) => { for (const fn of listeners) fn({ data, origin, source }) }
  const settled = async () => { for (let i = 0; i < 200 && els['rd-intake-open'].hidden && !els['rd-intake-msg'].textContent; i++) await new Promise((r) => setTimeout(r, 5)) }
  return { els, calls, everything, message, popup, settled }
}

await test('intake: the key leaves the address bar first, and only a non-extractable CryptoKey goes to the helper', async () => {
  const p = intakePage(`k=${KEY}&/docs/a.html#part`)
  assert.deepEqual(p.calls[0], ['history.replaceState', ['/e/amber-otter-quilt-river']], 'the first thing the script does')
  await p.settled()
  assert.equal(p.els['rd-intake-open'].hidden, false, p.els['rd-intake-msg'].textContent)
  p.els['rd-intake-open'].click()
  assert.deepEqual(p.calls.find((c) => c[0] === 'window.open'), ['window.open', [CONTENT + '/__redrop/e2e-intake', '_blank', 'popup,width=480,height=320']])
  // Messages from anywhere but the popup on the artifact's origin are ignored.
  p.message({ type: 'redrop-e2e-ready' }, 'https://evil.example')
  p.message({ type: 'redrop-e2e-ready' }, CONTENT, {})
  assert.equal(p.calls.filter((c) => c[0] === 'popup.postMessage').length, 0)
  p.message({ type: 'redrop-e2e-ready' })
  const sent = p.calls.filter((c) => c[0] === 'popup.postMessage')
  assert.equal(sent.length, 1)
  const [msg, origin] = sent[0][1] as [{ type: string; key: CryptoKey }, string]
  assert.equal(origin, CONTENT)
  assert.equal(msg.type, 'redrop-e2e-key')
  assert.ok(msg.key instanceof CryptoKey && !msg.key.extractable && msg.key.algorithm.name === 'HKDF')
  p.message({ type: 'redrop-e2e-stored' }, 'https://evil.example')
  assert.equal(p.calls.filter((c) => c[0] === 'location.replace').length, 0)
  p.message({ type: 'redrop-e2e-stored' })
  assert.deepEqual(p.calls.at(-1), ['location.replace', [CONTENT + '/#/docs/a.html#part']])
  assert.ok(!p.everything().includes(KEY) && !p.everything().includes(KEY.slice(0, 12)), 'the key text went somewhere')
})

await test('intake: a damaged or missing key opens nothing; a blocked window says so', async () => {
  for (const frag of [`k%3D${KEY}`, `k=${KEY}.`, `K=${KEY}`, `k=${KEY}&k=${KEY}`]) {
    const p = intakePage(frag)
    await p.settled()
    assert.match(p.els['rd-intake-msg'].textContent, /damaged/, frag)
    assert.equal(p.els['rd-intake-open'].hidden, true)
    assert.equal(p.calls[0][0], 'history.replaceState')
  }
  const missing = intakePage('')
  await missing.settled()
  assert.match(missing.els['rd-intake-msg'].textContent, /missing/)
  const blocked = intakePage(`k=${KEY}`, { blocked: true })
  await blocked.settled()
  blocked.els['rd-intake-open'].click()
  assert.match(blocked.els['rd-intake-msg'].textContent, /pop-ups/)
  assert.equal(blocked.els['rd-intake-open'].disabled, false)
})

function helperPage(opts: { opener?: boolean; storageFails?: boolean } = {}) {
  const sent: Array<[unknown, string]> = []
  const stored: Array<[string, unknown]> = []
  let closed = false
  const els = { 'rd-e2e-intake': new El({ 'data-app': APP }), 'rd-e2e-intake-msg': new El() }
  const listeners: Listener[] = []
  const opener = opts.opener === false ? null : { postMessage: (m: unknown, origin: string) => sent.push([m, origin]) }
  const context = vm.createContext({
    document: { getElementById: (id: keyof typeof els) => els[id] ?? null },
    crypto: globalThis.crypto, CryptoKey: globalThis.CryptoKey, TextEncoder, TextDecoder, atob, btoa, ReadableStream,
    opener, close: () => { closed = true },
    addEventListener: (type: string, fn: Listener) => { if (type === 'message') listeners.push(fn) },
  })
  ;(context as Record<string, unknown>).window = context
  vm.runInContext(lib, context)
  const E = (context as Record<string, unknown>).RedropE2E as Record<string, unknown>
  E.keys = {
    put: async (name: string, key: unknown) => {
      if (opts.storageFails) throw new Error('storage')
      stored.push([name, key])
    },
  }
  vm.runInContext(helper, context)
  const message = async (data: unknown, origin = APP, source: unknown = opener) => {
    for (const fn of listeners) fn({ data, origin, source })
    await new Promise((r) => setTimeout(r, 10))
  }
  return { els, sent, stored, message, closed: () => closed }
}

await test('helper: takes one non-extractable CryptoKey from the app origin\'s opener, stores it, answers and closes', async () => {
  const good = await crypto.subtle.importKey('raw', new Uint8Array(32), 'HKDF', false, ['deriveBits', 'deriveKey'])
  // WebCrypto refuses an extractable HKDF key, so an extractable AES key
  // stands in for "a key the page could read out".
  const extractable = await crypto.subtle.importKey('raw', new Uint8Array(32), 'AES-GCM', true, ['encrypt'])
  const p = helperPage()
  assert.deepEqual(plain(p.sent), [[{ type: 'redrop-e2e-ready' }, APP]])
  await p.message({ type: 'redrop-e2e-key', key: good }, 'https://evil.example')
  await p.message({ type: 'redrop-e2e-key', key: good }, APP, {})
  await p.message({ type: 'redrop-e2e-key', key: KEY })
  await p.message({ type: 'redrop-e2e-key', key: extractable })
  assert.deepEqual(p.stored, [], 'stored from a stranger, or not a non-extractable CryptoKey')
  assert.equal(p.sent.filter(([m]) => (m as { type: string }).type === 'redrop-e2e-refused').length, 2)
  await p.message({ type: 'redrop-e2e-key', key: good })
  assert.deepEqual(p.stored, [['intake', good]])
  assert.deepEqual(plain(p.sent.at(-1)), [{ type: 'redrop-e2e-stored' }, APP])
  assert.equal(p.closed(), true)
  await p.message({ type: 'redrop-e2e-key', key: good })
  assert.equal(p.stored.length, 1, 'only one key')
})

await test('helper: without an opener it does nothing; a storage failure is reported', async () => {
  const lone = helperPage({ opener: false })
  assert.match(lone.els['rd-e2e-intake-msg'].textContent, /only when an encrypted link opens it/)
  const good = await crypto.subtle.importKey('raw', new Uint8Array(32), 'HKDF', false, ['deriveBits', 'deriveKey'])
  const failing = helperPage({ storageFails: true })
  await failing.message({ type: 'redrop-e2e-key', key: good })
  assert.deepEqual(plain(failing.sent.at(-1)), [{ type: 'redrop-e2e-storage' }, APP])
  assert.equal(failing.closed(), false)
})
