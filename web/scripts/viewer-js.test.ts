// Unit test for the viewer page script (internal/content/static/viewer.js):
// the address bar follows the same-origin frame. It runs the real source in a
// VM against a minimal fake page, without a browser.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const source = readFileSync(new URL('../../internal/content/static/viewer.js', import.meta.url), 'utf8')
const ORIGIN = 'https://amber-otter-quilt-river.usercontent.example'

type Listener = () => void

function page(start = '/', src = '/') {
  const top = new URL(start, ORIGIN)
  const location = { origin: ORIGIN, pathname: top.pathname, search: top.search, hash: top.hash }
  const frameListeners: Record<string, Listener> = {}
  let win: { listeners: Record<string, Listener[]>; location: URL | null } = { listeners: {}, location: null }
  const frame = {
    src,
    getAttribute(k: string) { return k === 'src' ? src : null },
    addEventListener(type: string, fn: Listener) { frameListeners[type] = fn },
    get contentWindow() {
      return {
        get location() {
          if (!win.location || win.location.origin !== ORIGIN) throw new Error('SecurityError: cross-origin frame')
          return win.location
        },
        addEventListener(type: string, fn: Listener) {
          if (!win.location || win.location.origin !== ORIGIN) throw new Error('SecurityError: cross-origin frame')
          ;(win.listeners[type] ??= []).push(fn)
        },
      }
    },
  }
  const replaced: string[] = []
  const context = vm.createContext({
    window: { Intl },
    document: { querySelectorAll: () => [], getElementById: (id: string) => (id === 'rd-frame' ? frame : null) },
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
    location, URL, URLSearchParams, Date, Intl, setInterval, clearInterval, Array,
  })
  vm.runInContext(source, context)
  return {
    frame,
    replaced,
    // The frame loads a new document at url (a new window, no listeners).
    load(url: string) {
      win = { listeners: {}, location: new URL(url, ORIGIN) }
      frameListeners.load!()
    },
    // Same document: a fragment or history change inside the frame.
    fire(type: 'hashchange' | 'popstate', url: string) {
      win.location = new URL(url, ORIGIN)
      for (const fn of win.listeners[type] ?? []) fn()
    },
  }
}

await test('every frame load replaces the address bar with the frame path and query', () => {
  const p = page()
  p.load('/') // the first load: nothing changes
  p.load('/b.html?x=1')
  p.load('/docs/c.html')
  assert.deepEqual(p.replaced, ['/b.html?x=1', '/docs/c.html'])
})

await test('fragments and in-frame history are reflected', () => {
  const p = page()
  p.load('/a.html')
  p.fire('hashchange', '/a.html#part-2')
  p.fire('popstate', '/a.html')
  p.fire('popstate', '/a.html?tab=2')
  assert.deepEqual(p.replaced, ['/a.html', '/a.html#part-2', '/a.html', '/a.html?tab=2'])
})

await test('an older version shows as ?redrop-version=N; leaving it drops the parameter', () => {
  const p = page('/a.html?x=1&redrop-version=3', '/__redrop/v/3/a.html?x=1')
  p.load('/__redrop/v/3/a.html?x=1')
  p.load('/__redrop/v/3/sub/b.html')
  p.load('/root.html') // a root-absolute link leads to the current version
  assert.deepEqual(p.replaced, ['/sub/b.html?redrop-version=3', '/root.html'])
})

await test('another origin and redrop pages leave the address bar alone', () => {
  const p = page('/start.html')
  p.load('https://elsewhere.example/page') // reading its location throws
  p.load('/__redrop/open')
  p.load('/__redrop/v/0/x.html')
  assert.deepEqual(p.replaced, [])
})

await test('the visitor\'s fragment is handed to the frame', () => {
  const p = page('/guide.html#install', '/guide.html')
  assert.equal(p.frame.src, '/guide.html#install')
  p.load('/guide.html#install')
  assert.deepEqual(p.replaced, [])
})
