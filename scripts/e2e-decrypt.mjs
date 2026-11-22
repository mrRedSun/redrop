#!/usr/bin/env node
// Decrypts an end-to-end encrypted redrop link the way the viewer does
// (DESIGN.md "End-to-end encrypted links"), with Node's WebCrypto and the
// viewer's own decryptor, internal/content/static/e2e.js.
//
//   node scripts/e2e-decrypt.mjs [PATH] < link.txt    prints the file at PATH ("" = the start page)
//   node scripts/e2e-decrypt.mjs --list < link.txt    prints the version's manifest as JSON
//
// The full link, with #k=<key>, comes on stdin, never on the command line
// (shell history and process lists show arguments). The key stays in this
// process: the script sends the server only what a browser would (the
// page, the manifest, the object). Set REDROP_COOKIE to send a visit cookie
// for a password-protected or view-limited link. Exit codes: 0 ok, 1
// failure (including a wrong key or damaged content), 2 usage.
import { readFileSync } from 'node:fs'
import vm from 'node:vm'

const arg = process.argv[2] ?? ''
const link = readFileSync(0, 'utf8').trim()
if (!link.includes('#') || /\s/.test(link)) {
  console.error('usage: e2e-decrypt.mjs [PATH | --list] < file-with-the-full-link')
  process.exit(2)
}

const context = vm.createContext({ crypto: globalThis.crypto, TextEncoder, TextDecoder, atob, btoa, ReadableStream })
vm.runInContext(readFileSync(new URL('../internal/content/static/e2e.js', import.meta.url), 'utf8'), context)
const E = context.RedropE2E

const url = new URL(link)
const { key: keyText } = E.splitFragment(url.hash.slice(1))
if (!keyText) {
  console.error('the link has no #k= key')
  process.exit(2)
}
url.hash = ''
const cookie = process.env.REDROP_COOKIE ? { Cookie: process.env.REDROP_COOKIE } : {}

async function get(path, headers) {
  const res = await fetch(new URL(path, url), { headers: { ...cookie, ...headers }, redirect: 'manual' })
  if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`)
  return res
}

// The viewer page carries what the authenticated data needs.
const page = await (await get(url.pathname + url.search, { 'Sec-Fetch-Dest': 'document', 'Sec-Fetch-Mode': 'navigate', 'Sec-Fetch-Site': 'none', Accept: 'text/html' })).text()
const attr = (name) => {
  const m = new RegExp(`data-${name}="([^"]*)"`).exec(page)
  if (!m) throw new Error(`not an end-to-end viewer page (no data-${name}); is the link gated or plaintext?`)
  return m[1]
}
const artifact = attr('artifact'), version = Number(attr('version')), check = attr('key-check')

const key = await E.importKey(keyText)
if ((await E.keyCheck(key, artifact)) !== check) throw new Error("the link's key does not belong to this artifact")
const object = async (name) => new Uint8Array(await (await get(`/__redrop/v/${version}/${name}`, { 'Sec-Fetch-Dest': 'empty' })).arrayBuffer())
const manifest = E.parseManifest(await E.openBytes(await object('manifest'), key, E.manifestBinding(artifact, version)))

if (arg === '--list') {
  const { kind, title, entry, files } = manifest
  process.stdout.write(JSON.stringify({ artifact, version, kind, title, entry, files }, null, 2) + '\n')
} else {
  const r = E.resolve(manifest, arg)
  if (!r.file) throw new Error(`no file at ${JSON.stringify(arg)}`)
  process.stdout.write(await E.openBytes(await object(r.file.object), key, E.fileBinding(artifact, version, r.file.path)))
}
