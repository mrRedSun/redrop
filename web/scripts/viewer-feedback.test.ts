// Unit test for the feedback form in the viewer page script
// (internal/content/static/viewer.js, DESIGN.md "Viewer feedback"). It runs
// the real source in a VM against a fake page. The framed artifact is a
// proxy that records every property the script reads and refuses writes, so
// the tests can show that the artifact is only read: its selection and its
// location, never its DOM.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const source = readFileSync(new URL('../../internal/content/static/viewer.js', import.meta.url), 'utf8')
const ORIGIN = 'https://amber-otter-quilt-river.usercontent.example'

type Listener = (e?: unknown) => void

class El {
  listeners: Record<string, Listener[]> = {}
  hidden = false
  value = ''
  textContent = ''
  disabled = false
  attrs: Record<string, string> = {}
  children: unknown[] = []
  constructor(init: Partial<El> = {}) { Object.assign(this, init) }
  addEventListener(type: string, fn: Listener) { (this.listeners[type] ??= []).push(fn) }
  getAttribute(k: string) { return this.attrs[k] ?? null }
  contains(n: unknown) { return n === this || this.children.includes(n) }
  dispatch(type: string, e: unknown = {}) { for (const fn of this.listeners[type] ?? []) fn(e) }
}

// A document whose body holds one text node, with Range positions in it.
// wrap(doc) is what its nodes report as their ownerDocument.
function textDocument(text: string, wrap: (d: object) => object = (d) => d) {
  const raw: Record<string, unknown> = {}
  const doc = wrap(raw)
  const node = Object.freeze({ nodeType: 3, ownerDocument: doc, data: text })
  const body = Object.freeze({ nodeType: 1, ownerDocument: doc, childNodes: Object.freeze([node]) })
  const pos = (n: unknown, off: number) => (n === node ? off : n === body ? (off === 0 ? 0 : text.length) : NaN)
  class FakeRange {
    s = 0
    e = 0
    startContainer: unknown = node
    startOffset = 0
    endContainer: unknown = node
    endOffset = 0
    commonAncestorContainer: unknown = node
    setStart(n: unknown, off: number) { this.s = pos(n, off) }
    setEnd(n: unknown, off: number) { this.e = pos(n, off) }
    toString() { return this.e > this.s ? text.slice(this.s, this.e) : '' }
  }
  raw.body = body
  raw.documentElement = body
  raw.createRange = () => new FakeRange()
  // Selects text[from:to] in UTF-16 units, as a browser's offsets are.
  const select = (from: number, to: number) => {
    const r = new FakeRange()
    Object.assign(r, { s: from, e: to, startOffset: from, endOffset: to })
    return Object.freeze({ isCollapsed: from === to, rangeCount: 1, getRangeAt: (_i: number) => r })
  }
  return { doc, node, select }
}

// watch wraps target so that every read is recorded and every write fails.
function watch<T extends object>(target: T, reads: string[], writes: string[]): T {
  return new Proxy(target, {
    get(t, k, r) { reads.push(String(k)); return Reflect.get(t, k, r) },
    set(_t, k) { writes.push(String(k)); return false },
    defineProperty(_t, k) { writes.push(String(k)); return false },
    deleteProperty(_t, k) { writes.push(String(k)); return false },
  })
}

type Reply = { status: number; json?: unknown } | 'offline'

function page(opts: { text?: string; frameAt?: string | null; crossOrigin?: boolean; version?: number; current?: number; withForm?: boolean; start?: string } = {}) {
  const { text = 'Intro. The totals are wrong here. Outro.', frameAt = '/report.html#sec', crossOrigin = false, version = 1, current = 1, withForm = true, start = '/report.html#sec' } = opts
  const reads: string[] = []
  const writes: string[] = []
  // The artifact's document: reads are recorded, writes refused; its nodes
  // and selection are frozen, so writing to them throws in strict mode.
  const td = textDocument(text, (d) => watch(d, reads, writes))
  let selection: unknown = Object.freeze({ isCollapsed: true, rangeCount: 0 })
  const frameDoc = td.doc
  const frameWin = watch({
    get location() { if (crossOrigin) throw new Error('SecurityError'); return new URL(frameAt ?? '/', ORIGIN) },
    getSelection: () => { if (crossOrigin) throw new Error('SecurityError'); return selection },
    document: frameDoc,
  }, reads, writes)
  const frame = frameAt === null ? null : new El({ attrs: { src: '/' } })
  Object.defineProperty(frame ?? {}, 'contentWindow', { get: () => frameWin })

  const panel = new El()
  const els: Record<string, El> = {
    'rd-info': panel,
    'rd-fb': new El({ hidden: true, attrs: { 'data-version': String(version), 'data-current': String(current) } }),
    'rd-fb-body': new El(), 'rd-fb-name': new El(), 'rd-fb-send': new El(), 'rd-fb-status': new El(),
    'rd-fb-quote': new El({ hidden: true }), 'rd-fb-quote-text': new El(), 'rd-fb-unquote': new El(),
  }
  panel.children = Object.values(els)
  if (!withForm) for (const k of Object.keys(els)) if (k.startsWith('rd-fb')) delete els[k]
  if (frame) els['rd-frame'] = frame

  const sent: Array<{ url: string; init: { method: string; headers: Record<string, string>; body: string; credentials: string } }> = []
  let reply: Reply = { status: 201, json: { ok: true } }
  const fetch = (url: string, init: never) => {
    sent.push({ url, init })
    const r = reply
    if (r === 'offline') return Promise.reject(new TypeError('Failed to fetch'))
    return Promise.resolve({
      status: r.status,
      json: () => (r.json === undefined ? Promise.reject(new SyntaxError('not JSON')) : Promise.resolve(r.json)),
    })
  }
  // Array.from records the longest input it was given (m3: bounded work).
  let fromMax = 0
  const TrackedArray = {
    prototype: Array.prototype,
    from(x: ArrayLike<unknown> | Iterable<unknown>) {
      const a = Array.from(x as Iterable<unknown>)
      fromMax = Math.max(fromMax, typeof x === 'string' ? x.length : a.length)
      return a
    },
  }
  const top = new URL(start, ORIGIN)
  // The file page has no frame: the page's own selection is read.
  let ownSelection: unknown = Object.freeze({ isCollapsed: true, rangeCount: 0 })
  const context = vm.createContext({
    window: { Intl, getSelection: () => ownSelection },
    document: {
      visibilityState: 'visible',
      addEventListener() {},
      querySelectorAll: () => [],
      getElementById: (id: string) => els[id] ?? null,
    },
    location: { origin: ORIGIN, pathname: top.pathname, search: top.search, hash: '' },
    history: { state: null, replaceState() {}, pushState() { throw new Error('no pushState') } },
    URL, URLSearchParams, Date, Intl, Array: TrackedArray, fetch,
    setInterval: () => 0, clearInterval: () => {},
  })
  vm.runInContext(source, context)
  const flush = () => new Promise((r) => setImmediate(r))
  return {
    els, sent, reads, writes, td, panel,
    fromMax: () => fromMax,
    select(from: number, to: number) { selection = td.select(from, to) },
    selectOwn(sel: unknown) { ownSelection = sel },
    reply(r: Reply) { reply = r },
    open() { panel.dispatch('toggle', { newState: 'open' }) },
    async submit(body = 'Section 3 is wrong', name = '') {
      els['rd-fb-body']!.value = body
      els['rd-fb-name']!.value = name
      let prevented = false
      els['rd-fb']!.dispatch('submit', { preventDefault: () => { prevented = true } })
      assert(prevented, 'the form must never submit natively')
      for (let i = 0; i < 5; i++) await flush()
    },
    payload(i = 0) { return JSON.parse(sent[i]!.init.body) },
  }
}

const allowedReads = new Set(['location', 'getSelection', 'createRange', 'body', 'documentElement'])
function onlyRead(p: ReturnType<typeof page>) {
  assert.deepEqual(p.writes, [], 'the artifact must not be written to')
  for (const k of p.reads) assert(allowedReads.has(k), `the script read ${k} on the artifact`)
}

await test('the form shows once the script runs, and the selection in the frame becomes the quote', async () => {
  const p = page()
  assert.equal(p.els['rd-fb']!.hidden, false)
  const text = 'Intro. The totals are wrong here. Outro.'
  p.select(text.indexOf('totals'), text.indexOf(' here'))
  p.open()
  assert.equal(p.els['rd-fb-quote']!.hidden, false)
  assert.equal(p.els['rd-fb-quote-text']!.textContent, 'totals are wrong')
  await p.submit('Fix this', 'Sam')
  assert.equal(p.sent.length, 1)
  const { url, init } = p.sent[0]!
  assert.equal(url, '/__redrop/feedback')
  assert.equal(init.method, 'POST')
  assert.equal(init.credentials, 'same-origin')
  assert.equal(init.headers['Content-Type'], 'application/json')
  assert.deepEqual(p.payload(), {
    body: 'Fix this', name: 'Sam', version: 1, page: '/report.html#sec',
    quote: { exact: 'totals are wrong', prefix: 'Intro. The ', suffix: ' here. Outro.' },
  })
  assert.equal(p.els['rd-fb-status']!.textContent, 'Thanks, your feedback was sent.')
  assert.equal(p.els['rd-fb-body']!.value, '')
  assert.equal(p.els['rd-fb-quote']!.hidden, true)
  assert.equal(p.els['rd-fb-send']!.disabled, false)
  onlyRead(p)
})

await test('a long selection is shortened by code points and its suffix follows the shortened text', async () => {
  const sel = '😀'.repeat(10) + 'x'.repeat(600)
  const text = 'p'.repeat(100) + sel + 'after'
  const p = page({ text })
  p.select(100, 100 + sel.length)
  p.open()
  await p.submit()
  const q = p.payload().quote
  assert.equal(Array.from(q.exact).length, 500)
  assert.equal(q.exact, '😀'.repeat(10) + 'x'.repeat(490))
  assert.equal(q.prefix, 'p'.repeat(64))
  assert.equal(q.suffix, 'x'.repeat(64))
  assert.equal(text.indexOf(q.prefix + q.exact + q.suffix) >= 0, true, 'the triple is a text quote of the document')
  onlyRead(p)
})

await test('a huge page and selection: the quote is right and no array grows with them', async () => {
  const sel = 'q'.repeat(1_000_000)
  const text = 'a'.repeat(2_000_000) + sel + 'b'.repeat(2_000_000)
  const p = page({ text })
  p.select(2_000_000, 2_000_000 + sel.length)
  p.open()
  await p.submit()
  assert.deepEqual(p.payload().quote, { exact: 'q'.repeat(500), prefix: 'a'.repeat(64), suffix: 'q'.repeat(64) })
  assert(p.fromMax() <= 2 * (500 + 64), `Array.from got ${p.fromMax()} units`)
  onlyRead(p)
})

await test('no quote without a selection, for blank selections, and after "Don\'t quote"', async () => {
  const p = page()
  p.open()
  assert.equal(p.els['rd-fb-quote']!.hidden, true)
  p.select(6, 7) // a space
  p.open()
  assert.equal(p.els['rd-fb-quote']!.hidden, true)
  p.select(11, 17)
  p.open()
  assert.equal(p.els['rd-fb-quote']!.hidden, false)
  p.els['rd-fb-unquote']!.dispatch('click')
  assert.equal(p.els['rd-fb-quote']!.hidden, true)
  await p.submit()
  assert.equal('quote' in p.payload(), false)
  onlyRead(p)
})

await test('an older version: the page reads as the address bar shows it, with its version', async () => {
  const p = page({ frameAt: '/__redrop/v/2/a.html?x=1#s', version: 2, current: 3, start: '/a.html?x=1&redrop-version=2' })
  await p.submit()
  assert.deepEqual([p.payload().page, p.payload().version], ['/a.html?x=1&redrop-version=2#s', 2])
  // A root-absolute link inside it leads to the current version.
  const q = page({ frameAt: '/root.html', version: 2, current: 3, start: '/a.html?redrop-version=2' })
  await q.submit()
  assert.deepEqual([q.payload().page, q.payload().version], ['/root.html', 3])
})

await test('a frame on another origin: no quote, and the address bar and page version are used', async () => {
  const p = page({ crossOrigin: true, version: 4, current: 4, start: '/last.html?q=1' })
  p.open()
  assert.equal(p.els['rd-fb-quote']!.hidden, true)
  await p.submit()
  assert.deepEqual(p.payload(), { body: 'Section 3 is wrong', name: '', version: 4, page: '/last.html?q=1' })
})

await test('the file page quotes its own selection, but not text selected in the panel', async () => {
  const p = page({ frameAt: null, start: '/' })
  const td = textDocument('file preview text')
  p.selectOwn(td.select(5, 12))
  p.open()
  assert.equal(p.els['rd-fb-quote-text']!.textContent, 'preview')
  const inPanel = td.select(0, 4)
  ;(inPanel.getRangeAt(0) as { commonAncestorContainer: unknown }).commonAncestorContainer = p.els['rd-fb-status']
  p.selectOwn(inPanel)
  p.open()
  assert.equal(p.els['rd-fb-quote']!.hidden, true)
  await p.submit()
  assert.deepEqual([p.payload().page, p.payload().version], ['/', 1])
})

await test('failures show the server message or a fixed one, as text, and re-enable Send', async () => {
  const p = page()
  await p.submit('   ')
  assert.equal(p.sent.length, 0)
  assert.equal(p.els['rd-fb-status']!.textContent, 'Write a comment first.')
  for (const [reply, want] of [
    [{ status: 429, json: { error: 'rate_limited', message: '<b>Too much</b> feedback.' } }, '<b>Too much</b> feedback.'],
    [{ status: 403, json: { error: 'x', message: 42 } }, 'Feedback could not be sent.'],
    [{ status: 405 }, 'Feedback is not available on this page right now.'],
    ['offline', 'Could not reach the server. Check your connection and try again.'],
  ] as const) {
    p.reply(reply)
    await p.submit('kept')
    assert.equal(p.els['rd-fb-status']!.textContent, want)
    assert.equal(p.els['rd-fb-body']!.value, 'kept', 'a failed comment is kept for another try')
    assert.equal(p.els['rd-fb-send']!.disabled, false)
  }
})

await test('without the form (feedback off) nothing is read from the artifact and nothing is sent', () => {
  const p = page({ withForm: false })
  p.select(11, 17)
  p.open()
  assert.equal(p.sent.length, 0)
  assert.equal(p.reads.includes('getSelection'), false)
  onlyRead(p)
})
