// Unit test for the info button widget served on artifact pages
// (internal/content/infowidget.js). It runs the real source in a VM against
// a minimal fake DOM: enough to check isolation, contents, and toggling.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const source = readFileSync(new URL('../../internal/content/infowidget.js', import.meta.url), 'utf8')

type Listener = (e: unknown) => void

class FakeNode {
  children: FakeNode[] = []
  attrs: Record<string, string> = {}
  listeners: Record<string, Listener[]> = {}
  text = ''
  className = ''
  id = ''
  hidden = false
  type = ''
  alt = ''
  src = ''
  shadow: { node: FakeNode; mode: string } | undefined
  readonly tag: string
  constructor(tag: string) { this.tag = tag }
  get textContent(): string { return this.text + this.children.map((c) => c.textContent).join('') }
  set textContent(v: string) { this.text = v; this.children = [] }
  appendChild(c: FakeNode) { this.children.push(c); return c }
  setAttribute(k: string, v: string) { this.attrs[k] = v }
  getAttribute(k: string) { return this.attrs[k] }
  addEventListener(type: string, fn: Listener) { (this.listeners[type] ??= []).push(fn) }
  removeEventListener(type: string, fn: Listener) { this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn) }
  dispatch(type: string, e: unknown = {}) { for (const fn of [...(this.listeners[type] ?? [])]) fn(e) }
  focus() { this.focused = true }
  focused = false
  attachShadow(init: { mode: string }) {
    const root = Object.assign(new FakeNode('#shadow'), { activeElement: null as FakeNode | null })
    this.shadow = { node: root, mode: init.mode }
    return root
  }
  find(pred: (n: FakeNode) => boolean): FakeNode | undefined {
    if (pred(this)) return this
    for (const c of this.children) { const f = c.find(pred); if (f) return f }
    return undefined
  }
}

function run(data: Record<string, unknown>, opts: { framed?: boolean; noBody?: boolean } = {}) {
  const documentElement = new FakeNode('html')
  const body = new FakeNode('body')
  const doc = Object.assign(new FakeNode('#document'), {
    documentElement,
    body: opts.noBody ? null : body,
    createElement: (tag: string) => new FakeNode(tag),
  })
  const window: Record<string, unknown> = {}
  window.top = opts.framed ? {} : window
  const requests: string[] = []
  const intervals = new Set<number>()
  let next = 1
  const context = vm.createContext({
    window, document: doc,
    fetch: (u: string) => requests.push(u), XMLHttpRequest: function () { requests.push('xhr') },
    setInterval: () => { const id = next++; intervals.add(id); return id },
    clearInterval: (id: number) => intervals.delete(id),
  })
  const widget = vm.runInContext('(' + source + ')', context) as (d: unknown) => void
  widget(data)
  const host = (opts.noBody ? documentElement : body).children.find((c) => c.tag === 'redrop-info')
  return { window, doc, body, host, requests, intervals, again: () => widget(data) }
}

const hour = 3600_000
const base = { title: '<b>Report</b>', version: 3, published: new Date(Date.now() - 2 * 24 * hour).toISOString(), expires: null, password: false, size: 12_345, files: 3, mark: 'data:image/svg+xml;base64,AA==' }

await test('widget renders once, in a closed shadow root, without requests or globals beyond its guard', () => {
  const w = run(base)
  assert(w.host, 'host appended to body')
  assert.equal(w.host.shadow?.mode, 'closed')
  assert.deepEqual(Object.keys(w.window).filter((k) => k !== 'top'), [], 'guard is non-enumerable')
  assert.equal(w.window.__redropInfo, true)
  w.again()
  assert.equal(w.body.children.length, 1, 'double injection guarded')
  assert.deepEqual(w.requests, [])
  const root = w.host.shadow!.node
  const h2 = root.find((n) => n.tag === 'h2')!
  assert.equal(h2.textContent, '<b>Report</b>', 'title is text, never markup')
  assert(root.find((n) => n.tag === 'style'), 'styles scoped inside the shadow root')
  const text = root.textContent
  assert.match(text, /Version 3, published 2 days ago/)
  assert.match(text, /Never expires/)
  assert.match(text, /12\.3 kB in 3 files/)
  assert.match(text, /Shared with redrop/)
  assert.doesNotMatch(text, /view|Password/)
})

await test('widget does nothing inside frames', () => {
  const w = run(base, { framed: true })
  assert.equal(w.host, undefined)
  assert.equal(w.window.__redropInfo, undefined)
})

await test('widget falls back to the document element without a body', () => {
  assert(run(base, { noBody: true }).host)
})

await test('widget shows limits and a live expiry', () => {
  const root = run({ ...base, expires: new Date(Date.now() + 3 * 24 * hour + 60_000).toISOString(), max_views: 1, views_left: 0, password: true, files: 1, size: 999 }).host!.shadow!.node
  const btn = root.find((n) => n.tag === 'button')!
  btn.dispatch('click')
  const text = root.textContent
  assert.match(text, /Expires in 3 days/)
  assert.match(text, /One-time link: this was the only view\./)
  assert.match(text, /Password-protected/)
  assert.match(text, /999 B in 1 file(?!s)/)
  const many = run({ ...base, max_views: 5, views_left: 2 }).host!.shadow!.node
  assert.match(many.textContent, /2 of 5 views left/)
})

await test('button toggles the panel accessibly; Esc and outside clicks close it and drop listeners', () => {
  const w = run(base)
  const root = w.host!.shadow!.node
  const btn = root.find((n) => n.tag === 'button')!
  const panel = root.find((n) => n.attrs.role === 'dialog')!
  assert.equal(btn.attrs['aria-label'], 'About this shared page')
  assert.equal(btn.attrs['aria-controls'], panel.id)
  assert.equal(panel.hidden, true)

  btn.dispatch('click')
  assert.equal(panel.hidden, false)
  assert.equal(btn.attrs['aria-expanded'], 'true')
  assert.equal(w.intervals.size, 1, 'countdown runs only while open')
  assert.equal(w.doc.listeners.keydown?.length, 1)

  // Clicks inside the widget keep it open; outside ones close it.
  w.doc.dispatch('pointerdown', { composedPath: () => [btn, root, w.host] })
  assert.equal(panel.hidden, false)
  w.doc.dispatch('pointerdown', { composedPath: () => [w.body] })
  assert.equal(panel.hidden, true)
  assert.equal(w.intervals.size, 0)
  assert.equal(w.doc.listeners.pointerdown?.length, 0)
  assert.equal(w.doc.listeners.keydown?.length, 0)

  btn.dispatch('click')
  ;(root as FakeNode & { activeElement: FakeNode | null }).activeElement = btn
  w.doc.dispatch('keydown', { key: 'Escape' })
  assert.equal(panel.hidden, true)
  assert.equal(btn.attrs['aria-expanded'], 'false')
  assert(btn.focused, 'focus returns to the button')

  btn.dispatch('click')
  btn.dispatch('click')
  assert.equal(panel.hidden, true, 'the button closes it again')
})
