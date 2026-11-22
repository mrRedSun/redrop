// Unit test for redrop's end-to-end decryptor (internal/content/static/e2e.js,
// DESIGN.md "End-to-end encrypted links"). It decrypts every object the Go
// encryptor produced in internal/e2e/testdata/vectors.json, so the two
// implementations stay one format, and checks that every tampered, truncated
// or re-presented object fails closed.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import vm from 'node:vm'

type Binding = { type: number; artifact: string; version: number; path: string }
type Manifest = { kind: string; title: string; entry: string; byPath: Record<string, { path: string; object: string; type: string; size: number }> }
type Resolved = { file?: { path: string }; redirect?: string; notFound?: boolean; listing?: boolean }
export type RedropE2E = {
  BUFFER_LIMIT: number
  KEY_TTL_MS: number
  keys: {
    put(artifact: string, key: CryptoKey, now: number): Promise<void>
    get(artifact: string, now: number): Promise<CryptoKey | null>
    remove(artifact: string): Promise<void>
  }
  splitFragment(f: string): { key: string | null; rest: string }
  importKey(text: string): Promise<CryptoKey>
  keyCheck(key: CryptoKey, artifact: string): Promise<string>
  manifestBinding(artifact: string, version: number): Binding
  fileBinding(artifact: string, version: number, path: string): Binding
  openBytes(bytes: Uint8Array, key: CryptoKey, b: Binding): Promise<Uint8Array>
  openStream(s: ReadableStream<Uint8Array>, key: CryptoKey, b: Binding): ReadableStream<Uint8Array>
  parseManifest(bytes: Uint8Array): Manifest
  resolve(m: Manifest, rel: string): Resolved
}

const lib = readFileSync(new URL('../../internal/content/static/e2e.js', import.meta.url), 'utf8')
const vectors = JSON.parse(readFileSync(new URL('../../internal/e2e/testdata/vectors.json', import.meta.url), 'utf8')) as {
  format: number; segment_size: number; key: string; artifact: string; key_check: string
  objects: Array<{ name: string; type: number; version: number; path: string; plaintext_len: number; plaintext_sha256: string; plaintext?: string; object: string }>
}

/** Loads e2e.js into a fresh context, as a browser or worker would. */
export function loadE2E(extra: Record<string, unknown> = {}): RedropE2E {
  const context = vm.createContext({ crypto: globalThis.crypto, CryptoKey: globalThis.CryptoKey, TextEncoder, TextDecoder, atob, btoa, ReadableStream, ...extra })
  vm.runInContext(lib, context)
  return context.RedropE2E as RedropE2E
}

type Req = { result?: unknown; onsuccess?: () => void; onerror?: () => void; onupgradeneeded?: () => void }
type Tx = { oncomplete?: () => void; onerror?: () => void; onabort?: () => void; objectStore(n: string): unknown }

// A minimal IndexedDB: one database, requests answered asynchronously,
// transactions complete once their requests have.
function fakeIndexedDB() {
  const stores = new Map<string, Map<string, unknown>>()
  let version = 0
  return {
    stores,
    // planted makes the database exist already, without the key store, as
    // an artifact on the same origin could.
    planted() { version = 1 },
    open(_name: string, want?: number) {
      const req: Req = {}
      queueMicrotask(() => {
        const upgrade = want !== undefined ? want > version : version === 0
        if (upgrade) version = want ?? 1
        const db = {
          version,
          objectStoreNames: { contains: (n: string) => stores.has(n) },
          createObjectStore(n: string) { stores.set(n, new Map()) },
          close() {},
          transaction(n: string) {
            const data = stores.get(n)!
            const tx: Tx = { objectStore: () => store }
            let pending = 0
            const settle = () => queueMicrotask(() => { if (pending === 0) tx.oncomplete?.() })
            const request = (run: () => unknown) => {
              const r: Req = {}
              pending++
              queueMicrotask(() => { r.result = run(); r.onsuccess?.(); pending--; settle() })
              return r
            }
            const store = {
              put: (v: unknown, k: string) => request(() => void data.set(k, v)),
              get: (k: string) => request(() => data.get(k)),
              delete: (k: string) => request(() => void data.delete(k)),
            }
            settle()
            return tx
          },
        }
        req.result = db
        if (upgrade) req.onupgradeneeded?.()
        req.onsuccess?.()
      })
      return req
    },
  }
}

const E = loadE2E()
const bytes = (b64: string) => Uint8Array.from(Buffer.from(b64, 'base64'))
const hex = async (b: Uint8Array) => Buffer.from(await crypto.subtle.digest('SHA-256', b as Uint8Array<ArrayBuffer>)).toString('hex')
const binding = (v: (typeof vectors.objects)[number]) =>
  v.type === 1 ? E.manifestBinding(vectors.artifact, v.version) : E.fileBinding(vectors.artifact, v.version, v.path)
const rejects = async (p: Promise<unknown>, code: string, what: string) => {
  await assert.rejects(p, (e: { code?: string }) => e.code === code, what)
}

await test('every Go vector decrypts to its plaintext', async () => {
  assert.equal(vectors.format, 1)
  assert.equal(vectors.segment_size, 65536)
  const key = await E.importKey(vectors.key)
  assert.equal(await E.keyCheck(key, vectors.artifact), vectors.key_check)
  assert.notEqual(await E.keyCheck(key, 'aaaaaaaaaaaaaaaa'), vectors.key_check)
  for (const v of vectors.objects) {
    const plain = await E.openBytes(bytes(v.object), key, binding(v))
    assert.equal(plain.length, v.plaintext_len, v.name)
    assert.equal(await hex(plain), v.plaintext_sha256, v.name)
    if (v.plaintext !== undefined) assert.ok(Buffer.from(plain).equals(Buffer.from(v.plaintext, 'base64')), v.name)
  }
  const manifest = vectors.objects.find((v) => v.type === 1)!
  const m = E.parseManifest(await E.openBytes(bytes(manifest.object), key, binding(manifest)))
  assert.equal(m.entry, 'index.html')
  assert.equal(m.title, 'Vector')
  assert.deepEqual(Object.keys(m.byPath).sort(), vectors.objects.filter((v) => v.type === 2 && v.version === manifest.version).map((v) => v.path).sort())
})

await test('streamed decryption yields the same bytes, in segments', async () => {
  const key = await E.importKey(vectors.key)
  const v = vectors.objects.find((o) => o.name === 'two segments')!
  const obj = bytes(v.object)
  // Deliver the object in odd-sized pieces, as a network would.
  const stream = new ReadableStream<Uint8Array>({
    start(c) { for (let i = 0; i < obj.length; i += 7001) c.enqueue(obj.subarray(i, i + 7001)); c.close() },
  })
  const parts: Uint8Array[] = []
  const reader = E.openStream(stream, key, binding(v)).getReader()
  for (;;) {
    const r = await reader.read()
    if (r.done) break
    parts.push(r.value)
  }
  assert.deepEqual(parts.map((p) => p.length), [65536, 5])
  assert.equal(await hex(Buffer.concat(parts)), v.plaintext_sha256)
})

await test('tampered, truncated, extended or re-presented objects fail closed', async () => {
  const key = await E.importKey(vectors.key)
  const v = vectors.objects.find((o) => o.name === 'two segments')!
  const obj = bytes(v.object)
  const seg = 65536 + 16
  const flip = (i: number) => { const o = obj.slice(); o[i] ^= 1; return o }
  const b = binding(v)
  await rejects(E.openBytes(flip(0), key, b), 'format', 'magic')
  await rejects(E.openBytes(flip(3), key, b), 'format', 'format version')
  await rejects(E.openBytes(flip(5), key, b), 'auth', 'salt')
  await rejects(E.openBytes(flip(30), key, b), 'auth', 'first segment')
  await rejects(E.openBytes(flip(20 + seg - 1), key, b), 'auth', 'first tag')
  await rejects(E.openBytes(flip(obj.length - 1), key, b), 'auth', 'last tag')
  await rejects(E.openBytes(obj.slice(0, obj.length - 1), key, b), 'auth', 'last byte removed')
  await rejects(E.openBytes(obj.slice(0, 20 + seg), key, b), 'auth', 'last segment removed')
  await rejects(E.openBytes(obj.slice(0, 20), key, b), 'auth', 'header only')
  await rejects(E.openBytes(obj.slice(0, 10), key, b), 'format', 'too short')
  await rejects(E.openBytes(Buffer.concat([obj, Buffer.from([0])]), key, b), 'auth', 'byte appended')
  await rejects(E.openBytes(Buffer.concat([obj.subarray(0, 20), obj.subarray(20 + seg), obj.subarray(20, 20 + seg)]), key, b), 'auth', 'segments swapped')
  await rejects(E.openBytes(obj, key, E.fileBinding('zzzzzzzzzzzzzzzz', v.version, v.path)), 'auth', 'other artifact')
  await rejects(E.openBytes(obj, key, E.fileBinding(vectors.artifact, v.version + 1, v.path)), 'auth', 'other version')
  await rejects(E.openBytes(obj, key, E.fileBinding(vectors.artifact, v.version, 'data/full.bin')), 'auth', 'other path')
  await rejects(E.openBytes(obj, key, E.manifestBinding(vectors.artifact, v.version)), 'auth', 'as the manifest')
  const other = await E.importKey('A'.repeat(43))
  await rejects(E.openBytes(obj, other, b), 'auth', 'wrong key')
  // A whole object served for another path is refused too.
  const page = vectors.objects.find((o) => o.path === 'index.html')!
  await rejects(E.openBytes(bytes(page.object), key, E.fileBinding(vectors.artifact, 3, 'docs/café notes.md')), 'auth', 'object swapped between paths')

  // A damaged stream stops with an error after its last good segment.
  const damaged = flip(obj.length - 1)
  const reader = E.openStream(new ReadableStream({ start(c) { c.enqueue(damaged); c.close() } }), key, b).getReader()
  assert.equal((await reader.read()).value!.length, 65536)
  await assert.rejects(reader.read(), (e: { code?: string }) => e.code === 'auth')
})

await test('keys: only 43 canonical base64url characters', async () => {
  for (const bad of ['', 'A'.repeat(42), 'A'.repeat(44), '+'.repeat(43), vectors.key.slice(0, 42) + 'B']) {
    // The last character of 43 holds 2 unused bits; B sets one of them.
    await rejects(E.importKey(bad), 'key', `key ${JSON.stringify(bad)}`)
  }
})

await test('fragments: the key, then & and the page or the artifact\'s own fragment', () => {
  const key = 'A'.repeat(43)
  assert.deepEqual({ ...E.splitFragment(`k=${key}`) }, { key, rest: '' })
  assert.deepEqual({ ...E.splitFragment(`k=${key}&/docs/a.html?x=1#part`) }, { key, rest: '/docs/a.html?x=1#part' })
  assert.deepEqual({ ...E.splitFragment(`k=${key}&part`) }, { key, rest: 'part' })
  assert.deepEqual({ ...E.splitFragment('/docs/a.html#part') }, { key: null, rest: '/docs/a.html#part' })
  assert.deepEqual({ ...E.splitFragment('k=short') }, { key: null, rest: 'k=short' })
  assert.deepEqual({ ...E.splitFragment(`k=${key}x`) }, { key: null, rest: `k=${key}x` })
  assert.deepEqual({ ...E.splitFragment(`k=${'!'.repeat(43)}`) }, { key: null, rest: `k=${'!'.repeat(43)}` })
})

const object = (c: string) => c.repeat(26)
function manifestOf(m: object) {
  return E.parseManifest(new TextEncoder().encode(JSON.stringify(m)))
}

await test('paths resolve like a plain bundle, without listings or SPA fallback', () => {
  const m = manifestOf({
    format: 1, kind: 'bundle', title: 'Site', entry: 'index.html', files: [
      { path: 'index.html', object: object('a'), type: 'text/html', size: 1 },
      { path: 'docs/index.html', object: object('b'), type: 'text/html', size: 1 },
      { path: 'docs/café notes.md', object: object('c'), type: 'text/markdown', size: 1 },
      { path: 'assets/app.css', object: object('d'), type: 'text/css', size: 1 },
    ],
  })
  const at = (p: string) => { const r = E.resolve(m, p); return r.file ? r.file.path : r.redirect ? `-> ${r.redirect}` : r.listing ? 'listing' : 404 }
  assert.equal(at(''), 'index.html')
  assert.equal(at('index.html'), 'index.html')
  assert.equal(at('docs'), '-> docs/')
  assert.equal(at('docs/'), 'docs/index.html')
  assert.equal(at('docs/café notes.md'), 'docs/café notes.md')
  assert.equal(at('assets'), '-> assets/')
  assert.equal(at('assets/'), 'listing')
  assert.equal(at('./docs/../assets/app.css'), 'assets/app.css')
  assert.equal(at('../../index.html'), 'index.html')
  assert.equal(at('client/route'), 404)
  const noEntry = manifestOf({ format: 1, kind: 'bundle', title: '', entry: '', files: [{ path: 'a.txt', object: object('a'), type: 'text/plain', size: 1 }] })
  assert.equal(E.resolve(noEntry, '').listing, true)
  const file = manifestOf({ format: 1, kind: 'file', title: 'r.pdf', entry: 'r.pdf', files: [{ path: 'r.pdf', object: object('a'), type: 'application/pdf', size: 1 }] })
  assert.equal(E.resolve(file, '').file!.path, 'r.pdf')
  assert.equal(E.resolve(file, 'r.pdf').file!.path, 'r.pdf')
  assert.equal(E.resolve(file, 'other').notFound, true)
})

await test('manifests are checked before use', () => {
  const good = { format: 1, kind: 'html', title: '', entry: 'index.html', files: [{ path: 'index.html', object: object('a'), type: 'text/html', size: 1 }] }
  manifestOf(good)
  const bad: Array<[string, object]> = [
    ['format', { ...good, format: 2 }],
    ['kind', { ...good, kind: 'zip' }],
    ['no files', { ...good, files: [] }],
    ['absolute path', { ...good, entry: '/x', files: [{ ...good.files[0], path: '/x' }] }],
    ['dot dot', { ...good, entry: '../x', files: [{ ...good.files[0], path: '../x' }] }],
    ['reserved', { ...good, entry: '__redrop/x', files: [{ ...good.files[0], path: '__redrop/x' }] }],
    ['object name', { ...good, files: [{ ...good.files[0], object: 'manifest' }] }],
    ['missing entry', { ...good, entry: 'nope.html' }],
    ['page without entry', { ...good, entry: '' }],
  ]
  for (const [name, m] of bad) assert.throws(() => manifestOf(m), (e: { code?: string }) => e.code === 'manifest', name)
  assert.throws(() => E.parseManifest(new Uint8Array([0xff, 0xfe])), (e: { code?: string }) => e.code === 'manifest', 'not UTF-8 JSON')
})

await test('the browser key store keeps only CryptoKeys, and only while fresh', async () => {
  const idb = fakeIndexedDB()
  const K = loadE2E({ indexedDB: idb })
  const key = await K.importKey(vectors.key)
  const t0 = 1_000_000
  await K.keys.put(vectors.artifact, key, t0)
  assert.equal(await K.keys.get(vectors.artifact, t0 + K.KEY_TTL_MS - 1), key)
  assert.equal(await K.keys.get('other', t0), null)
  assert.equal(await K.keys.get(vectors.artifact, t0 + K.KEY_TTL_MS), null, 'expired')
  assert.equal(idb.stores.get('keys')!.size, 0, 'an expired key is forgotten')
  await K.keys.put(vectors.artifact, key, t0)
  assert.equal(await K.keys.get(vectors.artifact, t0 - 1), null, 'a clock that went back')
  idb.stores.get('keys')!.set(vectors.artifact, { key: vectors.key, at: t0 })
  assert.equal(await K.keys.get(vectors.artifact, t0), null, 'key text is never accepted')
  await K.keys.put(vectors.artifact, key, t0)
  await K.keys.remove(vectors.artifact)
  assert.equal(await K.keys.get(vectors.artifact, t0), null)
  await assert.rejects(loadE2E().keys.get(vectors.artifact, t0), (e: { code?: string }) => e.code === 'storage', 'no IndexedDB')
  // A database the artifact made under the same name, without the store,
  // is upgraded rather than left to hang the viewer.
  const planted = fakeIndexedDB()
  planted.planted()
  const P = loadE2E({ indexedDB: planted })
  await P.keys.put(vectors.artifact, key, t0)
  assert.equal(await P.keys.get(vectors.artifact, t0), key)
})
