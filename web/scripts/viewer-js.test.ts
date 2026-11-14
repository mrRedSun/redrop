// Unit test for the viewer page script (internal/content/static/viewer.js):
// the address bar follows the same-origin frame. It runs the real source in a
// VM against a minimal fake page, without a browser, and with fake timers.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const source = readFileSync(new URL('../../internal/content/static/viewer.js', import.meta.url), 'utf8')
const ORIGIN = 'https://amber-otter-quilt-river.usercontent.example'
const POLL_MS = 250

type Listener = () => void

// Event targets that, like a real window, drop removed listeners.
class Target {
  listeners: Record<string, Listener[]> = {}
  addEventListener(type: string, fn: Listener) { (this.listeners[type] ??= []).push(fn) }
  removeEventListener(type: string, fn: Listener) {
    this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn)
  }
  count(type: string) { return this.listeners[type]?.length ?? 0 }
  dispatch(type: string) { for (const fn of [...(this.listeners[type] ?? [])]) fn() }
}

// One document in the frame: its window and, optionally, its Navigation API.
class FrameWindow extends Target {
  location: URL
  navigation: Target | undefined
  constructor(url: string, withNavigation: boolean) {
    super()
    this.location = new URL(url, ORIGIN)
    this.navigation = withNavigation ? new Target() : undefined
  }
}

// Intervals that run only when the test advances the clock.
function fakeClock() {
  let next = 1
  const live = new Map<number, { fn: () => void; ms: number; due: number }>()
  let now = 0
  return {
    live,
    setInterval(fn: () => void, ms: number) { live.set(next, { fn, ms, due: now + ms }); return next++ },
    clearInterval(id: number) { live.delete(id) },
    tick(ms: number) {
      const end = now + ms
      for (;;) {
        const due = [...live.values()].filter((t) => t.due <= end).sort((a, b) => a.due - b.due)[0]
        if (!due) break
        now = due.due
        due.due += due.ms
        due.fn()
      }
      now = end
    },
  }
}

function page(opts: { start?: string; src?: string; navigation?: boolean } = {}) {
  const { start = '/', src = '/', navigation = false } = opts
  const top = new URL(start, ORIGIN)
  const location = { origin: ORIGIN, pathname: top.pathname, search: top.search, hash: top.hash }
  const frameListeners: Record<string, Listener> = {}
  let win: FrameWindow | null = null
  const crossOrigin = () => !win || win.location.origin !== ORIGIN
  const guard = () => { if (crossOrigin()) throw new Error('SecurityError: cross-origin frame') }
  const frame = {
    src,
    getAttribute(k: string) { return k === 'src' ? src : null },
    addEventListener(type: string, fn: Listener) { frameListeners[type] = fn },
    // Like a browser's WindowProxy for another origin, every property read
    // and call throws; same-origin, it is the frame's window.
    get contentWindow() {
      return {
        get location() { guard(); return win!.location },
        get navigation() { guard(); return win!.navigation },
        addEventListener(type: string, fn: Listener) { guard(); win!.addEventListener(type, fn) },
        removeEventListener(type: string, fn: Listener) { guard(); win!.removeEventListener(type, fn) },
      }
    },
  }
  const viewerDoc = Object.assign(new Target(), {
    visibilityState: 'visible',
    querySelectorAll: () => [],
    getElementById: (id: string) => (id === 'rd-frame' ? frame : null),
  })
  const clock = fakeClock()
  const replaced: string[] = []
  const context = vm.createContext({
    window: { Intl },
    document: viewerDoc,
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
    location, URL, URLSearchParams, Date, Intl, Array,
    setInterval: clock.setInterval, clearInterval: clock.clearInterval,
  })
  vm.runInContext(source, context)
  return {
    frame,
    replaced,
    clock,
    get win() { return win! },
    // The frame loads a new document at url (a new window, no listeners).
    load(url: string) {
      win = new FrameWindow(url, navigation)
      frameListeners.load!()
    },
    // Same document: a fragment or history change inside the frame that
    // fires an event (and, with the Navigation API, currententrychange).
    fire(type: 'hashchange' | 'popstate', url: string) {
      win!.location = new URL(url, ORIGIN)
      win!.dispatch(type)
      win!.navigation?.dispatch('currententrychange')
    },
    // history.pushState or replaceState inside the frame: the URL changes,
    // no window event fires; the Navigation API reports currententrychange.
    historyChange(url: string) {
      win!.location = new URL(url, ORIGIN)
      win!.navigation?.dispatch('currententrychange')
    },
    visibility(state: 'visible' | 'hidden') {
      viewerDoc.visibilityState = state
      viewerDoc.dispatch('visibilitychange')
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

await test('replaceState to a new fragment is mirrored after one poll', () => {
  const p = page()
  p.load('/app.html')
  p.historyChange('/app.html#tab=settings')
  p.clock.tick(POLL_MS - 1)
  assert.deepEqual(p.replaced, ['/app.html'])
  p.clock.tick(1)
  assert.deepEqual(p.replaced, ['/app.html', '/app.html#tab=settings'])
  p.clock.tick(POLL_MS * 4) // unchanged: nothing more
  assert.deepEqual(p.replaced, ['/app.html', '/app.html#tab=settings'])
})

await test('pushState to a new path is mirrored', () => {
  const p = page()
  p.load('/')
  p.historyChange('/users/42?sort=name')
  p.clock.tick(POLL_MS)
  p.historyChange('/users/42/edit')
  p.clock.tick(POLL_MS)
  assert.deepEqual(p.replaced, ['/users/42?sort=name', '/users/42/edit'])
})

await test('with the Navigation API, currententrychange is followed and nothing polls', () => {
  const p = page({ navigation: true })
  p.load('/')
  assert.equal(p.clock.live.size, 0)
  assert.equal(p.win.count('hashchange') + p.win.count('popstate'), 0)
  p.historyChange('/users/42')
  p.historyChange('/users/42#bio')
  assert.deepEqual(p.replaced, ['/users/42', '/users/42#bio'])
  // A reload moves the listener to the new document's navigation object.
  const first = p.win
  p.load('/next.html')
  assert.equal(first.navigation!.count('currententrychange'), 0)
  assert.equal(p.win.navigation!.count('currententrychange'), 1)
  assert.equal(p.clock.live.size, 0)
})

await test('nothing polls or listens while the page is hidden; showing it catches up', () => {
  for (const navigation of [false, true]) {
    const p = page({ navigation })
    p.load('/a.html')
    p.visibility('hidden')
    assert.equal(p.clock.live.size, 0)
    assert.equal(p.win.count('hashchange') + p.win.count('popstate'), 0)
    assert.equal(p.win.navigation?.count('currententrychange') ?? 0, 0)
    p.historyChange('/a.html#later')
    p.clock.tick(POLL_MS * 10)
    assert.deepEqual(p.replaced, ['/a.html'])
    // A load while hidden updates the bar once, but starts nothing.
    p.load('/b.html')
    assert.equal(p.clock.live.size, 0)
    assert.equal(p.win.navigation?.count('currententrychange') ?? 0, 0)
    p.historyChange('/b.html#x')
    p.visibility('visible')
    assert.deepEqual(p.replaced, ['/a.html', '/b.html', '/b.html#x'])
    assert.equal(navigation ? p.win.navigation!.count('currententrychange') : p.clock.live.size, 1)
  }
})

await test('a reload replaces the previous poller and listeners', () => {
  const p = page()
  p.load('/a.html')
  assert.equal(p.clock.live.size, 1)
  p.load('/b.html')
  p.load('/c.html')
  // One poller and one listener each, on the current document. (The old
  // documents' window listeners go away with them: by load time the frame's
  // WindowProxy already points at the new window.)
  assert.equal(p.clock.live.size, 1)
  assert.equal(p.win.count('hashchange'), 1)
  assert.equal(p.win.count('popstate'), 1)
  p.historyChange('/c.html#x')
  p.clock.tick(POLL_MS)
  assert.deepEqual(p.replaced, ['/a.html', '/b.html', '/c.html', '/c.html#x'])
})

await test('an older version shows as ?redrop-version=N; leaving it drops the parameter', () => {
  const p = page({ start: '/a.html?x=1&redrop-version=3', src: '/__redrop/v/3/a.html?x=1' })
  p.load('/__redrop/v/3/a.html?x=1')
  p.load('/__redrop/v/3/sub/b.html')
  p.historyChange('/__redrop/v/3/sub/b.html#s2')
  p.clock.tick(POLL_MS)
  p.load('/root.html') // a root-absolute link leads to the current version
  assert.deepEqual(p.replaced, ['/sub/b.html?redrop-version=3', '/sub/b.html?redrop-version=3#s2', '/root.html'])
})

await test('another origin and redrop pages leave the address bar alone', () => {
  const p = page({ start: '/start.html' })
  p.load('https://elsewhere.example/page') // reading its location throws
  assert.equal(p.clock.live.size, 0)
  p.load('/__redrop/open')
  p.load('/__redrop/v/0/x.html')
  assert.deepEqual(p.replaced, [])
})

await test('a frame that leaves for another origin between loads is ignored without throwing', () => {
  for (const navigation of [false, true]) {
    const p = page({ navigation })
    p.load('/a.html')
    p.win.location = new URL('https://elsewhere.example/page')
    assert.doesNotThrow(() => p.clock.tick(POLL_MS * 3))
    assert.doesNotThrow(() => p.visibility('hidden'))
    assert.doesNotThrow(() => p.visibility('visible'))
    assert.deepEqual(p.replaced, ['/a.html'])
  }
})

await test('the visitor\'s fragment is handed to the frame', () => {
  const p = page({ start: '/guide.html#install', src: '/guide.html' })
  assert.equal(p.frame.src, '/guide.html#install')
  p.load('/guide.html#install')
  assert.deepEqual(p.replaced, [])
})
