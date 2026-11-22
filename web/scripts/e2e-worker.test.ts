// Unit test for redrop's decrypting service worker
// (internal/content/static/e2e-sw.js, DESIGN.md "End-to-end encrypted links",
// "Viewing"). It runs the worker script exactly as the server serves it
// (e2e.js, then the worker) in a VM with a fake worker scope and a fake
// server holding the Go vectors' ciphertext.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

const read = (p: string) => readFileSync(new URL(p, import.meta.url), 'utf8')
const script = read('../../internal/content/static/e2e.js') + '\n' + read('../../internal/content/static/e2e-sw.js')
const vectors = JSON.parse(read('../../internal/e2e/testdata/vectors.json')) as {
  key: string; artifact: string
  objects: Array<{ type: number; version: number; path: string; plaintext?: string; object: string }>
}
const ORIGIN = 'https://amber-otter-quilt-river.usercontent.example'
const SECURITY = {
  'Content-Security-Policy': "frame-ancestors 'self' https://redrop.example.com",
  'Referrer-Policy': 'no-referrer',
  'X-Content-Type-Options': 'nosniff',
  'Cache-Control': 'private, no-cache',
}

type Listener = (e: unknown) => void
type Client = { type: string; frameType: string; url?: string; posted?: unknown[]; postMessage?: (m: unknown) => void }

// The stored objects of every vector version, by "<version>/<name>": each
// manifest and its files, under the object names the manifest gives them.
function storedObjects(): Map<string, Uint8Array> {
  const bytes = (b64: string) => Uint8Array.from(Buffer.from(b64, 'base64'))
  const out = new Map<string, Uint8Array>()
  for (const manifest of vectors.objects.filter((o) => o.type === 1)) {
    const v = manifest.version
    out.set(`${v}/manifest`, bytes(manifest.object))
    const m = JSON.parse(Buffer.from(manifest.plaintext!, 'base64').toString('utf8')) as { files: Array<{ path: string; object: string }> }
    for (const f of m.files) out.set(`${v}/${f.object}`, bytes(vectors.objects.find((o) => o.version === v && o.path === f.path)!.object))
  }
  return out
}

async function worker(opts: { serve?: (path: string) => Response | null; clients?: Client[] } = {}) {
  const listeners: Record<string, Listener[]> = {}
  const fetched: string[] = []
  const objects = storedObjects()
  const clients = opts.clients ?? []
  const scope = vm.createContext({
    location: new URL(ORIGIN + '/__redrop/e2e-sw.js'),
    addEventListener(type: string, fn: Listener) { (listeners[type] ??= []).push(fn) },
    skipWaiting: () => Promise.resolve(),
    clients: {
      claim: () => Promise.resolve(),
      matchAll: () => Promise.resolve(clients),
      get: (id: string) => Promise.resolve(clients.find((c) => (c as { id?: string }).id === id)),
    },
    crypto: globalThis.crypto, CryptoKey: globalThis.CryptoKey, TextEncoder, TextDecoder, atob, btoa,
    ReadableStream, Response, Headers, URL, Map, setTimeout, clearTimeout,
    fetch: async (url: string) => {
      const path = new URL(url, ORIGIN).pathname
      fetched.push(path)
      const custom = opts.serve?.(path)
      if (custom) return custom
      const m = /^\/__redrop\/v\/([0-9]+)\/(.+)$/.exec(path)
      const body = m && objects.get(`${m[1]}/${m[2]}`)
      if (!body) return new Response('Not found', { status: 404 })
      return new Response(body as Uint8Array<ArrayBuffer>, { status: 200, headers: { 'Content-Type': 'application/octet-stream', ...SECURITY } })
    },
  })
  // A worker's global object is self.
  ;(scope as Record<string, unknown>).self = scope
  vm.runInContext(script, scope)
  const dispatch = (type: string, e: unknown) => { for (const fn of listeners[type] ?? []) fn(e) }
  const key = await crypto.subtle.importKey('raw', Buffer.from(vectors.key, 'base64url'), 'HKDF', false, ['deriveBits', 'deriveKey'])
  return {
    fetched,
    giveKey(source: Client = { type: 'window', frameType: 'top-level' }, artifact = vectors.artifact) {
      dispatch('message', { data: { type: 'redrop-e2e-key', key, artifact }, source })
    },
    // request returns the worker's response, or null when it lets the
    // request go to the network.
    async request(path: string, init: { mode?: string; destination?: string; clientId?: string; method?: string } = {}) {
      const box: { answer?: Promise<Response> } = {}
      dispatch('fetch', {
        request: { url: ORIGIN + path, method: init.method ?? 'GET', mode: init.mode ?? 'cors', destination: init.destination ?? '' },
        clientId: init.clientId ?? '',
        respondWith(p: Promise<Response>) { box.answer = p },
      })
      return box.answer ? await box.answer : null
    },
  }
}

const text = async (r: Response | null) => (r ? Buffer.from(await r.arrayBuffer()).toString('utf8') : '')
const plainOf = (path: string) => Buffer.from(vectors.objects.find((o) => o.path === path)!.plaintext!, 'base64').toString('utf8')

await test('a frame request gets the decrypted file with the artifact\'s security headers', async () => {
  const w = await worker()
  w.giveKey()
  const r = (await w.request('/__redrop/v/3/', { mode: 'navigate', destination: 'iframe' }))!
  assert.equal(r.status, 200)
  assert.equal(await text(r), plainOf('index.html'))
  for (const [k, v] of Object.entries(SECURITY)) assert.equal(r.headers.get(k), v, k)
  assert.equal(r.headers.get('content-type'), 'application/octet-stream', 'the manifest\'s type')
  assert.match(r.headers.get('content-disposition')!, /^inline; filename\*=UTF-8''index\.html$/)
  // Paths are encoded in URLs and decoded for the manifest.
  const u = (await w.request('/__redrop/v/3/docs/caf%C3%A9%20notes.md'))!
  assert.equal(await text(u), plainOf('docs/café notes.md'))
  assert.ok(w.fetched.every((p) => /^\/__redrop\/v\/3\/(manifest|[a-z2-7]{26})$/.test(p)), 'only stored names are fetched')
  const d = (await w.request('/__redrop/v/3/data/full.bin?download'))!
  assert.match(d.headers.get('content-disposition')!, /^attachment; filename\*=UTF-8''full\.bin$/)
})

await test('folders redirect to their slash form; there are no listings', async () => {
  const w = await worker()
  w.giveKey()
  const r = (await w.request('/__redrop/v/3/docs?x=1'))!
  assert.equal(r.status, 301)
  assert.equal(r.headers.get('location'), ORIGIN + '/__redrop/v/3/docs/?x=1')
  const listing = (await w.request('/__redrop/v/3/docs/'))!
  assert.equal(listing.status, 404)
  assert.match(await text(listing), /File listings are not available/)
  assert.equal((await w.request('/__redrop/v/3/missing.html'))!.status, 404)
})

await test('a root-absolute subresource resolves in the version of its page', async () => {
  const page = { id: 'frame-1', type: 'window', frameType: 'nested', url: ORIGIN + '/__redrop/v/3/index.html' }
  const w = await worker({ clients: [page] })
  w.giveKey()
  const r = (await w.request('/data/two.bin', { clientId: 'frame-1', destination: 'script' }))!
  assert.equal(r.status, 200)
  assert.equal((await r.arrayBuffer()).byteLength, 65541)
})

await test('top-level pages, other origins and other methods go to the network', async () => {
  const w = await worker()
  w.giveKey()
  assert.equal(await w.request('/', { mode: 'navigate', destination: 'document' }), null)
  assert.equal(await w.request('/__redrop/v/3/index.html', { mode: 'navigate', destination: 'document' }), null, 'the server redirects these to the viewer')
  assert.equal(await w.request('/__redrop/viewer.js', { clientId: 'x' }), null)
  assert.equal(await w.request('/__redrop/v/3/form', { method: 'POST' }), null)
  assert.equal(w.fetched.length, 0)
})

await test('damaged or swapped content answers an error page and none of its bytes', async () => {
  let swap = ''
  const w = await worker({
    serve: (path) => {
      if (!swap || !path.endsWith(swap)) return null
      // Serve two.bin's ciphertext for index.html's object.
      const two = vectors.objects.find((o) => o.path === 'data/two.bin')!
      return new Response(Buffer.from(two.object, 'base64'), { status: 200, headers: SECURITY })
    },
  })
  w.giveKey()
  // The object name of index.html, from the fetches of a first request.
  await w.request('/__redrop/v/3/index.html')
  swap = w.fetched[w.fetched.length - 1].split('/').pop()!
  const r = (await w.request('/__redrop/v/3/index.html'))!
  assert.equal(r.status, 502)
  const body = await text(r)
  assert.match(body, /integrity check/)
  assert.ok(!body.includes('hello'))
  assert.match(r.headers.get('content-security-policy')!, /default-src 'none'/)
})

await test('the server\'s gates decide: a miss or an ended visit is passed on', async () => {
  const w = await worker({ serve: (path) => (path.endsWith('/manifest') ? new Response('access required', { status: 403 }) : null) })
  w.giveKey()
  const r = (await w.request('/__redrop/v/3/'))!
  assert.equal(r.status, 403)
  assert.match(await text(r), /access to this link has ended/)
  const gone = await worker({ serve: () => new Response('Not found', { status: 404 }) })
  gone.giveKey()
  assert.equal((await gone.request('/__redrop/v/3/'))!.status, 404)
})

await test('a worker without a key asks the open viewer pages, and only top-level ones are believed', async () => {
  let asked: () => void = () => {}
  const askedOnce = new Promise<void>((resolve) => { asked = resolve })
  const viewer: Client = { type: 'window', frameType: 'top-level', posted: [] }
  viewer.postMessage = (m) => { viewer.posted!.push(m); asked() }
  const w = await worker({ clients: [viewer] })
  // A key from a frame (the artifact itself) is ignored.
  w.giveKey({ type: 'window', frameType: 'nested' }, 'aaaaaaaaaaaaaaaa')
  const pending = w.request('/__redrop/v/3/index.html')
  await askedOnce
  assert.deepEqual(JSON.parse(JSON.stringify(viewer.posted)), [{ type: 'redrop-e2e-need-key' }])
  w.giveKey(viewer)
  const r = (await pending)!
  assert.equal(await text(r), plainOf('index.html'))
})

await test('a file artifact that is an image is shown in an <img>, where an SVG\'s script can\'t run', async () => {
  const w = await worker()
  w.giveKey()
  const page = (await w.request('/__redrop/v/4/', { mode: 'navigate', destination: 'iframe' }))!
  assert.equal(page.status, 200)
  assert.match(page.headers.get('content-type')!, /^text\/html/)
  assert.match(page.headers.get('content-security-policy')!, /^default-src 'none'; img-src 'self'/)
  const html = await text(page)
  assert.match(html, /<img src="\/__redrop\/v\/4\/" alt="logo.svg">/)
  assert.ok(!html.includes('<script'), 'the wrapper runs nothing')
  assert.ok(!w.fetched.some((p) => /[a-z2-7]{26}$/.test(p)), 'the SVG is not fetched for the page')
  // The <img> request gets the file itself; a download gets it as an attachment.
  const img = (await w.request('/__redrop/v/4/', { destination: 'image' }))!
  assert.equal(img.headers.get('content-type'), 'image/svg+xml; charset=utf-8')
  assert.match(await text(img), /<svg/)
  const dl = (await w.request('/__redrop/v/4/logo.svg?download', { mode: 'navigate', destination: 'iframe' }))!
  assert.match(dl.headers.get('content-disposition')!, /^attachment/)
})
