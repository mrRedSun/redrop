// Unit test for the viewer page script (internal/content/static/viewer.js):
// the address bar follows only this page's own frame. It runs the real
// source in a VM against a minimal fake page.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const source = readFileSync(new URL('../../internal/content/static/viewer.js', import.meta.url), 'utf8')

function page(version = '0') {
  let now = 1_000_000
  const listeners: Record<string, () => void> = {}
  const frame = {
    attrs: { 'data-stream': '/__redrop/location?s=t', 'data-version': version } as Record<string, string>,
    getAttribute(k: string) { return this.attrs[k] ?? null },
    addEventListener(type: string, fn: () => void) { listeners[type] = fn },
  }
  const streams: Array<{ onmessage?: (e: { data: string }) => void }> = []
  const replaced: string[] = []
  const location = { origin: 'https://amber-otter-quilt-river.usercontent.example', pathname: '/', search: '' }
  class FakeDate extends Date {
    static override now() { return now }
  }
  const context = vm.createContext({
    window: { EventSource: true, Intl },
    document: { querySelectorAll: () => [], getElementById: (id: string) => (id === 'rd-frame' ? frame : null) },
    EventSource: class {
      onmessage?: (e: { data: string }) => void
      constructor(url: string) { assert.equal(url, '/__redrop/location?s=t'); streams.push(this) }
    },
    history: {
      state: null,
      replaceState(_s: unknown, _t: string, url: string) {
        replaced.push(url)
        const u = new URL(url, location.origin)
        location.pathname = u.pathname
        location.search = u.search
      },
    },
    location, URL, Date: FakeDate, Intl, setInterval, clearInterval, Array,
  })
  vm.runInContext(source, context)
  return {
    report: (path: string) => streams[0]!.onmessage!({ data: path }),
    load: () => listeners.load!(),
    wait: (ms: number) => { now += ms },
    replaced,
  }
}

await test('a navigation in this frame moves the address bar when the frame loads', () => {
  const p = page()
  p.report('/b.html?x=1')
  assert.deepEqual(p.replaced, [], 'nothing before the frame loads')
  p.wait(300)
  p.load()
  assert.deepEqual(p.replaced, ['/b.html?x=1'])
})

await test('a report arriving just after the frame loaded still applies', () => {
  const p = page()
  p.load()
  p.wait(200)
  p.report('/c.html')
  assert.deepEqual(p.replaced, ['/c.html'])
})

await test('a nested frame or another tab never sets the address bar', () => {
  const p = page()
  p.load() // initial load
  p.wait(5000)
  p.report('/nested.html') // a nested frame navigated; this frame does not load
  p.wait(6000) // the report expires
  p.load() // a later, unreported load (e.g. scripted) must not use it
  assert.deepEqual(p.replaced, [])
  p.wait(5000)
  p.report('/other-tab.html') // another tab's report arrives while this frame is idle
  p.wait(10000)
  assert.deepEqual(p.replaced, [])
})

await test('a load is used once; foreign paths are ignored; the old version stays selected', () => {
  const p = page('3')
  p.load()
  p.wait(100)
  p.report('/a.html')
  p.report('/b.html') // a second report for the same load does not apply at once
  assert.deepEqual(p.replaced, ['/a.html?redrop-version=3'])
  for (const bad of ['//evil.example/x', '/\\evil.example', 'https://evil.example/', 'javascript:alert(1)']) {
    p.report(bad)
    p.load()
  }
  assert.deepEqual(p.replaced, ['/a.html?redrop-version=3'])
})
