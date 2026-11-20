// Unit test for live reload in the viewer page script
// (internal/content/static/viewer.js, DESIGN.md "Live reload"). It runs the
// real source in a VM against a small fake DOM and a fake EventSource. The
// framed artifact's window is a proxy that refuses writes, so the tests show
// that live reload only reads it, reloads it and scrolls it.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const source = readFileSync(new URL('../../internal/content/static/viewer.js', import.meta.url), 'utf8')
const ORIGIN = 'https://amber-otter-quilt-river.usercontent.example'

type Listener = (e?: unknown) => void

// A minimal element: attributes, children, text, listeners.
class Node {
  parent: Node | null = null
  children: Node[] = []
  attrs: Record<string, string> = {}
  listeners: Record<string, Listener[]> = {}
  hidden = false
  className = ''
  text = ''
  tag: string
  constructor(tag: string, init: { id?: string; attrs?: Record<string, string>; text?: string } = {}) {
    this.tag = tag
    if (init.id) this.attrs.id = init.id
    Object.assign(this.attrs, init.attrs ?? {})
    if (init.text) this.textContent = init.text
  }
  get textContent(): string { return this.tag === '#text' ? this.text : this.children.map((c) => c.textContent).join('') }
  set textContent(v: string) {
    this.children = []
    if (this.tag === '#text') this.text = v
    else if (v) this.appendChild(new Node('#text', { text: v }))
  }
  get firstChild() { return this.children[0] ?? null }
  get nextSibling() {
    const sibs = this.parent?.children ?? []
    return sibs[sibs.indexOf(this) + 1] ?? null
  }
  getAttribute(k: string) { return k in this.attrs ? this.attrs[k] : null }
  setAttribute(k: string, v: string) { this.attrs[k] = String(v) }
  removeAttribute(k: string) { delete this.attrs[k] }
  appendChild(n: Node) { n.parent = this; this.children.push(n); return n }
  insertBefore(n: Node, ref: Node | null) {
    n.parent = this
    const i = ref ? this.children.indexOf(ref) : -1
    if (i < 0) this.children.push(n)
    else this.children.splice(i, 0, n)
    return n
  }
  removeChild(n: Node) { this.children = this.children.filter((c) => c !== n); n.parent = null; return n }
  addEventListener(type: string, fn: Listener) { (this.listeners[type] ??= []).push(fn) }
  dispatch(type: string, e: unknown = {}) { for (const fn of [...(this.listeners[type] ?? [])]) fn(e) }
  *walk(): Generator<Node> { for (const c of this.children) { yield c; yield* c.walk() } }
  matches(sel: string): boolean {
    const attr = /^\[([a-z-]+)\]$/.exec(sel)
    if (attr) return attr[1] in this.attrs
    if (sel === '.rd-tag') return this.className.split(' ').includes('rd-tag') || this.attrs.class === 'rd-tag'
    if (sel === '.rd-revs') return this.attrs.class === 'rd-revs'
    if (sel === '.rd-revs > li') return this.tag === 'li' && this.parent?.attrs.class === 'rd-revs'
    return false
  }
  querySelectorAll(sel: string) { return [...this.walk()].filter((n) => n.matches(sel)) }
  querySelector(sel: string) { return this.querySelectorAll(sel)[0] ?? null }
  // The rows' text, with tags marked, as a viewer reads them.
  get rendered() { return this.textContent.replace(/\s+/g, ' ').trim() }
}

class FakeEventSource {
  static last: FakeEventSource | null = null
  listeners: Record<string, Listener[]> = {}
  readyState = 0
  closed = false
  url: string
  constructor(url: string) { this.url = url; FakeEventSource.last = this }
  addEventListener(type: string, fn: Listener) { (this.listeners[type] ??= []).push(fn) }
  close() { this.closed = true; this.readyState = 2 }
  emit(type: string, data?: unknown) {
    for (const fn of this.listeners[type] ?? []) fn(data === undefined ? {} : { data: typeof data === 'string' ? data : JSON.stringify(data) })
  }
}

type Frame = { url: string; scrollX: number; scrollY: number }

function page(opts: { live?: boolean; version?: number; frameAt?: string; feedback?: boolean } = {}) {
  const { live = true, version = 1, frameAt = '/docs/a.html?x=1#s', feedback = true } = opts
  const body = new Node('body')
  const facts = body.appendChild(new Node('ul'))
  const row = live ? facts.appendChild(new Node('li', { id: 'rd-live', attrs: { 'data-src': '/__redrop/live', 'data-version': String(version) } })) : null
  if (row) row.hidden = true
  const shown = facts.appendChild(new Node('li', { id: 'rd-shown', text: `Version ${version}, published ` }))
  shown.appendChild(new Node('span', { attrs: { 'data-rel': '2026-01-01T00:00:00Z' } }))
  const size = facts.appendChild(new Node('li', { id: 'rd-size', text: '1 KB in 1 file' }))
  const revs = body.appendChild(new Node('ol', { attrs: { class: 'rd-revs' } }))
  const rev = revs.appendChild(new Node('li', { attrs: { 'data-rev': String(version), 'aria-current': 'page' }, text: `Version ${version}` }))
  const tag = new Node('span', { text: 'current' })
  tag.className = 'rd-tag'
  rev.appendChild(new Node('#text', { text: ' ' }))
  rev.appendChild(tag)
  const form = feedback ? body.appendChild(new Node('form', { id: 'rd-fb', attrs: { 'data-version': String(version), 'data-current': String(version) } })) : null

  // The frame: its document's window. Reads are allowed; writes throw.
  const writes: string[] = []
  const calls: string[] = []
  let doc: Frame = { url: frameAt, scrollX: 0, scrollY: 0 }
  const frameEl = new Node('iframe', { id: 'rd-frame', attrs: { src: '/docs/a.html?x=1' } })
  const location = () => {
    const u = new URL(doc.url, ORIGIN)
    return { origin: u.origin, pathname: u.pathname, search: u.search, hash: u.hash, href: u.href, reload() { calls.push('reload ' + u.pathname + u.search + u.hash) } }
  }
  const win = new Proxy({}, {
    get(_t, k) {
      if (new URL(doc.url, ORIGIN).origin !== ORIGIN) throw new Error('SecurityError: cross-origin frame')
      switch (k) {
        case 'location': return location()
        case 'scrollX': return doc.scrollX
        case 'scrollY': return doc.scrollY
        case 'scrollTo': return (x: number, y: number) => { calls.push(`scrollTo ${x},${y}`); doc.scrollX = x; doc.scrollY = y }
        case 'navigation': return undefined
        case 'addEventListener': case 'removeEventListener': return () => {}
      }
      return undefined
    },
    set(_t, k) { writes.push(String(k)); return false },
    defineProperty(_t, k) { writes.push(String(k)); return false },
  })
  Object.defineProperty(frameEl, 'contentWindow', { get: () => win })

  const ids: Record<string, Node | null> = { 'rd-live': row, 'rd-shown': shown, 'rd-size': size, 'rd-fb': form, 'rd-frame': frameEl }
  const viewerDoc = Object.assign(new Node('#document'), {
    visibilityState: 'visible',
    // The feedback form's own controls are inert stand-ins here.
    getElementById: (id: string) => ids[id] ?? (form && id.startsWith('rd-fb-') ? (ids[id] = new Node('div', { id })) : null),
    createElement: (tag: string) => new Node(tag),
    createTextNode: (text: string) => new Node('#text', { text }),
  })
  viewerDoc.appendChild(body)
  FakeEventSource.last = null
  const top = new URL('/docs/a.html?x=1', ORIGIN)
  const context = vm.createContext({
    window: { Intl, EventSource: FakeEventSource },
    document: viewerDoc,
    history: { state: null, replaceState() {}, pushState() { throw new Error('the viewer must never push history entries') } },
    location: { origin: ORIGIN, pathname: top.pathname, search: top.search, hash: top.hash },
    URL, URLSearchParams, Date, Intl, Array, JSON, Number, String,
    setInterval: () => 0, clearInterval: () => {},
  })
  vm.runInContext(source, context)
  return {
    es: FakeEventSource.last as FakeEventSource | null,
    row, shown, size, revs, form, calls, writes,
    frame: doc,
    // The frame finishes loading a document (a reload keeps the URL).
    load(next: Partial<Frame> = {}) {
      doc = { url: doc.url, scrollX: 0, scrollY: 0, ...next }
      frameEl.dispatch('load')
    },
    scroll(x: number, y: number) { doc.scrollX = x; doc.scrollY = y },
    navigate(url: string) { doc = { url, scrollX: 0, scrollY: 0 } },
  }
}

const v = (version: number) => ({ version, published: '2026-01-02T03:04:05Z', size: '3 KB', files: 1 })

await test('without the live row, nothing connects', () => {
  const p = page({ live: false })
  assert.equal(p.es, null)
})

await test('the stream opens; the row shows only while it is open', () => {
  const p = page()
  assert.equal(p.es!.url, '/__redrop/live')
  assert.equal(p.row!.hidden, true)
  p.es!.emit('open')
  assert.equal(p.row!.hidden, false)
  // A dropped stream reconnects by itself: the row stays.
  p.es!.readyState = 0
  p.es!.emit('error')
  assert.equal(p.row!.hidden, false)
  // Closed for good (refused): the row goes.
  p.es!.readyState = 2
  p.es!.emit('error')
  assert.equal(p.row!.hidden, true)
})

await test('"end" closes the stream and hides the row', () => {
  const p = page()
  p.es!.emit('open')
  p.es!.emit('end', {})
  assert.equal(p.es!.closed, true)
  assert.equal(p.row!.hidden, true)
})

await test('a new version reloads the frame in place: same path, query and fragment', () => {
  const p = page()
  p.es!.emit('version', v(1)) // the version shown: nothing to do
  assert.deepEqual(p.calls, [])
  p.es!.emit('version', v(2))
  assert.deepEqual(p.calls, ['reload /docs/a.html?x=1#s'])
  p.load()
  p.es!.emit('version', v(2)) // already shown
  assert.deepEqual(p.calls, ['reload /docs/a.html?x=1#s'])
  assert.deepEqual(p.writes, [])
})

await test('the frame\'s current page is reloaded, wherever it navigated', () => {
  const p = page()
  p.navigate('/other/b.html?tab=2')
  p.es!.emit('version', v(2))
  assert.deepEqual(p.calls, ['reload /other/b.html?tab=2'])
})

await test('the scroll position is restored when the browser left the page at the top', () => {
  const p = page()
  p.scroll(0, 800)
  p.es!.emit('version', v(2))
  p.load({ scrollY: 0 })
  assert.deepEqual(p.calls, ['reload /docs/a.html?x=1#s', 'scrollTo 0,800'])

  // The browser restored it itself: left alone.
  p.scroll(10, 300)
  p.es!.emit('version', v(3))
  p.load({ scrollX: 10, scrollY: 300 })
  assert.deepEqual(p.calls.slice(2), ['reload /docs/a.html?x=1#s'])

  // A page at the top stays at the top.
  p.scroll(0, 0)
  p.es!.emit('version', v(4))
  p.load()
  assert.deepEqual(p.calls.slice(3), ['reload /docs/a.html?x=1#s'])
  assert.deepEqual(p.writes, [])
})

await test('a version that arrives during a reload reloads once more after it', () => {
  const p = page()
  p.es!.emit('version', v(2))
  p.es!.emit('version', v(3))
  p.es!.emit('version', v(4))
  assert.equal(p.calls.length, 1)
  p.load()
  assert.equal(p.calls.length, 2)
  p.load()
  assert.equal(p.calls.length, 2)
})

await test('another origin, or redrop\'s own pages in the frame, are not reloaded', () => {
  for (const at of ['https://elsewhere.example/x', '/__redrop/v/1/docs/a.html']) {
    const p = page({ frameAt: at })
    assert.doesNotThrow(() => p.es!.emit('version', v(2)))
    assert.deepEqual(p.calls, [], at)
  }
})

await test('malformed events are ignored', () => {
  const p = page()
  for (const data of ['not json', 'null', '{"version":"2"}', '{}']) p.es!.emit('version', data)
  assert.deepEqual(p.calls, [])
})

await test('the panel follows: version line, size, feedback version, current mark', () => {
  const p = page({ version: 1 })
  p.es!.emit('version', v(2))
  assert.match(p.shown.rendered, /^Version 2, published/)
  assert.equal(p.shown.querySelector('[data-rel]')!.getAttribute('data-rel'), '2026-01-02T03:04:05Z')
  assert.equal(p.size.rendered, '3 KB in 1 file')
  assert.equal(p.form!.getAttribute('data-version'), '2')
  assert.equal(p.form!.getAttribute('data-current'), '2')
  const rows = p.revs.children
  assert.equal(rows.length, 2)
  assert.match(rows[0].rendered, /^Version 2 current ·/)
  assert.equal(rows[0].getAttribute('aria-current'), 'page')
  assert.equal(rows[1].rendered, 'Version 1')
  assert.equal(rows[1].getAttribute('aria-current'), null)

  // A rollback marks the existing row.
  p.load()
  p.es!.emit('version', { ...v(1), files: 2 })
  assert.equal(p.size.rendered, '3 KB in 2 files')
  assert.equal(p.revs.children.length, 2)
  assert.match(p.revs.children[0].rendered, /^Version 2 · \S/) // its time, relative
  assert.equal(p.revs.children[1].rendered, 'Version 1 current')
  assert.equal(p.revs.children[1].getAttribute('aria-current'), 'page')
})
