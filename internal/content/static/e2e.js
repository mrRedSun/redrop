// redrop end-to-end encryption, format 1 (DESIGN.md "End-to-end encrypted
// links", "Format"). It decrypts the objects the CLI encrypts with
// internal/e2e, reads a version's manifest and resolves paths in it. It only
// decrypts. The viewer page loads it as /__redrop/e2e.js, the service worker
// script starts with it, and the web tests run it against the Go vectors in
// internal/e2e/testdata. It exposes RedropE2E on the global object.
;(function (root) {
  'use strict'
  var MAGIC = [0x52, 0x44, 0x45, 0x01]
  var SALT = 16
  var HEADER = 4 + SALT
  var SEGMENT = 65536
  var TAG = 16
  var CHUNK = SEGMENT + TAG
  var KEY_TEXT = 43
  var TYPE_MANIFEST = 1
  var TYPE_FILE = 2
  // Objects up to this size are decrypted whole before any byte is
  // released; larger ones stream, one authenticated segment at a time.
  var BUFFER_LIMIT = 64 * 1024 * 1024
  var OBJECT_NAME = /^[a-z2-7]{26}$/
  var KEY_CHARS = /^[A-Za-z0-9_-]+$/
  var KINDS = { html: true, file: true, bundle: true }
  var utf8 = new TextEncoder()

  function fail(code, message) {
    var e = new Error(message)
    e.code = code // format, auth, key, manifest
    return e
  }
  function subtle() {
    var c = root.crypto
    if (!c || !c.subtle) throw fail('unsupported', 'WebCrypto is not available here')
    return c.subtle
  }

  function concat(a, b) {
    var out = new Uint8Array(a.length + b.length)
    out.set(a, 0)
    out.set(b, a.length)
    return out
  }

  function fromB64url(s) {
    var b64 = s.replace(/-/g, '+').replace(/_/g, '/')
    while (b64.length % 4) b64 += '='
    var bin = atob(b64)
    var out = new Uint8Array(bin.length)
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
    return out
  }
  function toB64url(bytes) {
    var bin = ''
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i])
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  }

  // splitFragment splits a link fragment (without "#") into the key text
  // and the rest: "k=<43 characters>", optionally followed by "&" and the
  // rest. A fragment without a key is all rest. Any fragment that could
  // hold a key, whole or damaged (keyAttempt), is a key attempt: unless it
  // is exactly that form it gives {damaged: true} and no rest, so no part of
  // it can reach the frame (review 33 re-check B-2b'). A rest that holds
  // any 12 characters of the key in a row, in either case and however split
  // from the rest, is damaged too (a second, damaged copy of the link).
  // internal/e2e's SplitFragment follows the same rule;
  // internal/e2e/testdata/fragments.json holds the cases both are tested
  // against.
  function splitFragment(fragment) {
    var key = fragment.slice(2, 2 + KEY_TEXT), rest = fragment.slice(2 + KEY_TEXT)
    if (fragment.slice(0, 2) === 'k=' && key.length === KEY_TEXT && KEY_CHARS.test(key)) {
      if (rest === '') return { key: key, rest: '', damaged: false }
      var own = rest.slice(1)
      if (rest.charAt(0) === '&' && !keyAttempt(own) && !holdsKeyText(own, key)) return { key: key, rest: own, damaged: false }
      return { key: null, rest: '', damaged: true }
    }
    if (keyAttempt(fragment)) return { key: null, rest: '', damaged: true }
    return { key: null, rest: fragment, damaged: false }
  }

  // decodeLenient percent-decodes s as often as that changes it (at most
  // MAX_DECODES times). Only escapes of ASCII characters are decoded; any
  // other "%" is kept, so a malformed escape doesn't stop the rest.
  var MAX_DECODES = 8
  var ASCII_ESCAPE = /%([0-7][0-9A-Fa-f])/g
  function decodeLenient(s) {
    for (var i = 0; i < MAX_DECODES; i++) {
      var d = s.replace(ASCII_ESCAPE, function (_, hex) { return String.fromCharCode(parseInt(hex, 16)) })
      if (d === s) break
      s = d
    }
    return s
  }
  // keyText is s as keyAttempt reads it: decoded, with every non-ASCII
  // character and every escape left (of a non-ASCII byte) as a space.
  var NON_ASCII = /%[89A-Fa-f][0-9A-Fa-f]|[^\x00-\x7F]/gu
  function keyText(s) { return decodeLenient(s).replace(NON_ASCII, ' ') }
  // keyAttempt reports whether s could hold a key, whole or damaged: in
  // keyText, "k" in any case at the start or after a character that is not
  // a letter or digit, then up to 3 characters that are neither, then "="
  // or ":", or a run of at least MIN_KEY_RUN key characters with both upper
  // and lower case letters (a key that lost its "k="; lower-case anchors and
  // file names don't match).
  var MIN_KEY_RUN = 40
  var K_PARAM = /(^|[^A-Za-z0-9])[kK][^A-Za-z0-9=:]{0,3}[=:]/
  var KEY_RUN = /[A-Za-z0-9_-]{40,}/g
  function keyAttempt(s) {
    var d = keyText(s)
    if (K_PARAM.test(d)) return true
    var runs = d.match(KEY_RUN) || []
    for (var i = 0; i < runs.length; i++) {
      if (runs[i].length >= MIN_KEY_RUN && /[A-Z]/.test(runs[i]) && /[a-z]/.test(runs[i])) return true
    }
    return false
  }
  // holdsKeyText reports whether s, read as keyAttempt reads it, holds any
  // 12 characters of key in a row, in either case.
  function holdsKeyText(s, key) {
    var d = keyText(s).toLowerCase(), k = key.toLowerCase()
    for (var i = 0; i + 12 <= k.length; i++) if (d.indexOf(k.slice(i, i + 12)) >= 0) return true
    return false
  }
  // queryKeyAttempt reports whether a raw query (without "?") could hold a
  // key, by keyAttempt, once redrop's own parameters are taken out: a link
  // whose "#" became "?" (review 33 re-check B-2b″).
  var OWN_PARAMS = { 'redrop-e2e': true, 'redrop-version': true, 'redrop-info': true }
  function queryKeyAttempt(raw) {
    return keyAttempt(raw.split('&').filter(function (p) { return !OWN_PARAMS[p.split('=')[0]] }).join('&'))
  }
  // hasEncodedFragment reports a path that holds a "#", encoded once or
  // more: a link whose fragment, key included, went into the path (review
  // 33 re-check M3).
  function hasEncodedFragment(path) { return decodeLenient(path).indexOf('#') >= 0 }

  // importKey turns a key in its link form into a non-extractable HKDF key.
  function importKey(text) {
    return Promise.resolve().then(function () {
      if (typeof text !== 'string' || text.length !== KEY_TEXT || !KEY_CHARS.test(text)) {
        throw fail('key', 'malformed key')
      }
      var raw = fromB64url(text)
      if (raw.length !== 32 || toB64url(raw) !== text) throw fail('key', 'malformed key')
      return subtle().importKey('raw', raw, 'HKDF', false, ['deriveBits', 'deriveKey'])
    })
  }

  // keyCheck is the value the server stores to recognise the artifact's key.
  function keyCheck(key, artifact) {
    var info = concat(utf8.encode('redrop-e2e/1 key-check\0'), utf8.encode(artifact))
    return subtle().deriveBits({ name: 'HKDF', hash: 'SHA-256', salt: new Uint8Array(0), info: info }, key, 128)
      .then(function (bits) { return toB64url(new Uint8Array(bits)) })
  }

  function objectKey(key, salt) {
    return subtle().deriveKey(
      { name: 'HKDF', hash: 'SHA-256', salt: salt, info: utf8.encode('redrop-e2e/1 object') },
      key, { name: 'AES-GCM', length: 256 }, false, ['decrypt'])
  }

  function manifestBinding(artifact, version) {
    return { type: TYPE_MANIFEST, artifact: artifact, version: version, path: '' }
  }
  function fileBinding(artifact, version, path) {
    return { type: TYPE_FILE, artifact: artifact, version: version, path: path }
  }

  // aad is header ‖ type ‖ u16 len ‖ artifact ‖ u32 version ‖ u16 len ‖ path.
  function aad(header, b) {
    var id = utf8.encode(b.artifact), path = utf8.encode(b.path)
    if (b.type !== TYPE_MANIFEST && b.type !== TYPE_FILE) throw fail('format', 'unknown object type')
    if (!(b.version >= 1 && b.version <= 0xffffffff) || id.length > 0xffff || path.length > 0xffff) {
      throw fail('format', 'binding out of range')
    }
    var out = new Uint8Array(header.length + 1 + 2 + id.length + 4 + 2 + path.length)
    var v = new DataView(out.buffer), o = 0
    out.set(header, o); o += header.length
    out[o++] = b.type
    v.setUint16(o, id.length); o += 2
    out.set(id, o); o += id.length
    v.setUint32(o, b.version); o += 4
    v.setUint16(o, path.length); o += 2
    out.set(path, o)
    return out
  }

  // nonce is uint88(i) ‖ last.
  function nonce(i, last) {
    var n = new Uint8Array(12), v = new DataView(n.buffer)
    v.setUint32(3, Math.floor(i / 0x100000000))
    v.setUint32(7, i >>> 0)
    n[11] = last ? 1 : 0
    return n
  }

  // segments yields an object's plaintext, one authenticated segment at a
  // time. read() returns {value, done} like a ReadableStream reader.
  async function* segments(read, key, b) {
    // buf[off:] is read but not yet used. A whole object read at once is
    // never copied; a stream keeps at most one segment and one read.
    var buf = new Uint8Array(0), off = 0, done = false
    async function fill(n) {
      while (buf.length - off < n && !done) {
        var r = await read()
        if (r.done) done = true
        else if (r.value && r.value.length) {
          buf = concat(buf.subarray(off), r.value)
          off = 0
        }
      }
    }
    function take(n) {
      var s = buf.subarray(off, off + n)
      off += s.length
      return s
    }
    await fill(HEADER)
    if (buf.length - off < HEADER) throw fail('format', 'not an encrypted object')
    var header = take(HEADER).slice()
    for (var m = 0; m < 4; m++) if (header[m] !== MAGIC[m]) throw fail('format', 'not an encrypted object')
    var data = aad(header, b)
    var k = await objectKey(key, header.subarray(4))
    for (var i = 0; ; i++) {
      await fill(CHUNK + 1)
      var last = buf.length - off <= CHUNK
      var seg = take(CHUNK)
      if (seg.length < TAG) throw fail('auth', 'truncated object')
      var plain
      try {
        plain = await subtle().decrypt({ name: 'AES-GCM', iv: nonce(i, last), additionalData: data, tagLength: 128 }, k, seg)
      } catch (e) {
        throw fail('auth', 'the object failed authentication')
      }
      if (last && plain.byteLength === 0 && i > 0) throw fail('auth', 'empty final segment')
      yield new Uint8Array(plain)
      if (last) return
    }
  }

  function bytesReader(bytes) {
    var given = false
    return function () {
      if (given) return Promise.resolve({ done: true })
      given = true
      return Promise.resolve({ done: false, value: bytes })
    }
  }

  // openBytes decrypts a whole object: all of it, or an error and nothing.
  async function openBytes(bytes, key, b) {
    var parts = [], total = 0
    for await (var seg of segments(bytesReader(bytes), key, b)) {
      parts.push(seg)
      total += seg.length
    }
    var out = new Uint8Array(total), o = 0
    for (var p = 0; p < parts.length; p++) { out.set(parts[p], o); o += parts[p].length }
    return out
  }

  // openStream decrypts a ReadableStream of an object into a ReadableStream
  // of plaintext. A damaged object ends it with an error, after its last
  // authenticated segment.
  function openStream(stream, key, b) {
    var reader = stream.getReader()
    var it = segments(function () { return reader.read() }, key, b)
    return new ReadableStream({
      pull: function (controller) {
        return it.next().then(function (r) {
          if (r.done) controller.close()
          else controller.enqueue(r.value)
        }, function (e) {
          reader.cancel().catch(function () {})
          controller.error(e)
        })
      },
      cancel: function () { return reader.cancel() },
    })
  }

  // cleanPath is Go's path.Clean on "/"+p, without the leading slash: ""
  // for the root.
  function cleanPath(p) {
    var out = []
    p.split('/').forEach(function (seg) {
      if (seg === '' || seg === '.') return
      if (seg === '..') out.pop()
      else out.push(seg)
    })
    return out.join('/')
  }

  function validPath(p) {
    return typeof p === 'string' && p !== '' && p.length <= 1024 && p.indexOf('\0') < 0 &&
      p.indexOf('\\') < 0 && cleanPath(p) === p && p !== '__redrop' && p.indexOf('__redrop/') !== 0
  }

  // parseManifest decodes and checks a manifest's plaintext.
  function parseManifest(bytes) {
    var m
    try {
      m = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes))
    } catch (e) {
      throw fail('manifest', 'unreadable manifest')
    }
    if (!m || m.format !== 1 || !KINDS[m.kind] || !Array.isArray(m.files) || !m.files.length ||
      typeof m.entry !== 'string' || typeof m.title !== 'string') {
      throw fail('manifest', 'invalid manifest')
    }
    var byPath = Object.create(null), objects = Object.create(null)
    m.files.forEach(function (f) {
      if (!f || !validPath(f.path) || !OBJECT_NAME.test(f.object) || typeof f.type !== 'string' || !f.type ||
        typeof f.size !== 'number' || !(f.size >= 0) || byPath[f.path] || objects[f.object]) {
        throw fail('manifest', 'invalid manifest entry')
      }
      byPath[f.path] = f
      objects[f.object] = true
    })
    if (m.entry !== '' && !byPath[m.entry]) throw fail('manifest', 'invalid manifest entry')
    if (m.entry === '' && m.kind !== 'bundle') throw fail('manifest', 'invalid manifest entry')
    return { kind: m.kind, title: m.title, entry: m.entry, files: m.files, byPath: byPath }
  }

  function hasFilesUnder(m, dir) {
    for (var p in m.byPath) if (p.indexOf(dir) === 0) return true
    return false
  }

  // resolve maps a decoded request path inside a version ("" for its root)
  // onto the manifest, like the server resolves a plain artifact: {file},
  // {redirect: "dir/"}, or {notFound, listing} (a folder without a start
  // page: listings need the server to read names, so they're gone).
  function resolve(m, rel) {
    var trailing = rel === '' || rel.charAt(rel.length - 1) === '/'
    var p = cleanPath(rel)
    if (m.kind !== 'bundle') {
      return p === '' || p === m.entry ? { file: m.byPath[m.entry] } : { notFound: true }
    }
    if (p === '') return m.entry ? { file: m.byPath[m.entry] } : { notFound: true, listing: true }
    if (m.byPath[p]) return { file: m.byPath[p] }
    var index = m.byPath[p + '/index.html']
    if (index) return trailing ? { file: index } : { redirect: p + '/' }
    if (hasFilesUnder(m, p + '/')) return trailing ? { notFound: true, listing: true } : { redirect: p + '/' }
    return { notFound: true }
  }

  // ---- the browser's key store ----
  // The viewer keeps an artifact's key only as a non-extractable CryptoKey,
  // in IndexedDB, so it survives the viewer's own reload and the visitor's
  // reloads for KEY_TTL_MS after the full link was last opened. A
  // same-origin script can use such a key while it runs, but can't read its
  // bytes (DESIGN.md "End-to-end encrypted links", "Keys and links").
  var KEY_DB = 'redrop-e2e', KEY_STORE = 'keys'
  var KEY_TTL_MS = 12 * 60 * 60 * 1000
  // keyDB opens the database with its store. The artifact shares this
  // origin's IndexedDB and may have made a database of the same name
  // without the store; the store is then added in a version upgrade.
  function keyDB(idb, version) {
    return new Promise(function (resolve, reject) {
      var open
      try { open = version ? idb.open(KEY_DB, version) : idb.open(KEY_DB) } catch (e) { reject(fail('storage', 'IndexedDB is not available')); return }
      open.onupgradeneeded = function () {
        if (!open.result.objectStoreNames.contains(KEY_STORE)) open.result.createObjectStore(KEY_STORE)
      }
      open.onerror = open.onblocked = function () { reject(fail('storage', 'IndexedDB refused')) }
      open.onsuccess = function () {
        var db = open.result
        if (db.objectStoreNames.contains(KEY_STORE)) { resolve(db); return }
        var next = db.version + 1
        db.close()
        if (version) { reject(fail('storage', 'IndexedDB refused')); return }
        keyDB(idb, next).then(resolve, reject)
      }
    })
  }
  function keyTx(mode, run) {
    var idb = root.indexedDB
    if (!idb) return Promise.reject(fail('storage', 'IndexedDB is not available'))
    return keyDB(idb, 0).then(function (db) {
      return new Promise(function (resolve, reject) {
        var out, tx
        try {
          tx = db.transaction(KEY_STORE, mode)
          run(tx.objectStore(KEY_STORE), function (v) { out = v })
        } catch (e) {
          db.close()
          reject(fail('storage', 'IndexedDB refused'))
          return
        }
        tx.oncomplete = function () { db.close(); resolve(out) }
        tx.onerror = tx.onabort = function () { db.close(); reject(fail('storage', 'IndexedDB refused')) }
      })
    })
  }
  var keys = {
    // put keeps key for artifact from now on.
    put: function (artifact, key, now) {
      return keyTx('readwrite', function (s) { s.put({ key: key, at: now }, artifact) })
    },
    // get returns artifact's key while it is fresh, else null (and forgets it).
    get: function (artifact, now) {
      return keyTx('readwrite', function (s, set) {
        var r = s.get(artifact)
        r.onsuccess = function () {
          var v = r.result
          if (v && v.key instanceof CryptoKey && now >= v.at && now - v.at < KEY_TTL_MS) set(v.key)
          else if (v) s.delete(artifact)
        }
      }).then(function (k) { return k || null })
    },
    remove: function (artifact) {
      return keyTx('readwrite', function (s) { s.delete(artifact) })
    },
  }

  var api = {
    BUFFER_LIMIT: BUFFER_LIMIT,
    KEY_TTL_MS: KEY_TTL_MS,
    keys: keys,
    splitFragment: splitFragment,
    keyAttempt: keyAttempt,
    queryKeyAttempt: queryKeyAttempt,
    hasEncodedFragment: hasEncodedFragment,
    decodeLenient: decodeLenient,
    importKey: importKey,
    keyCheck: keyCheck,
    manifestBinding: manifestBinding,
    fileBinding: fileBinding,
    openBytes: openBytes,
    openStream: openStream,
    parseManifest: parseManifest,
    resolve: resolve,
    cleanPath: cleanPath,
  }
  root.RedropE2E = api
})(typeof self !== 'undefined' ? self : globalThis)
