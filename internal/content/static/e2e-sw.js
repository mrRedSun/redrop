// redrop's decrypting service worker for end-to-end encrypted artifacts
// (DESIGN.md "End-to-end encrypted links", "Viewing"). The server serves it
// as /__redrop/e2e-sw.js, after e2e.js, only on an end-to-end encrypted
// artifact's host and only past its gates. The viewer page registers it
// with scope /__redrop/v/, so it never controls the viewer page or sees a
// top-level navigation.
//
// For a request from the frame for /__redrop/v/<N>/<path>, it decrypts
// version N's manifest, resolves the path, fetches and decrypts the object,
// and answers with the manifest's type and the ciphertext response's
// security headers. It has no cache: every object comes from the server,
// through the gates, so a revoked link stops at once. The key lives in
// memory only; after the browser stops the worker, it asks an open viewer
// page again. It accepts keys only from top-level windows.
;(function () {
  'use strict'
  var E = self.RedropE2E
  var VERSION_PATH = /^\/__redrop\/v\/([1-9][0-9]*)\/(.*)$/
  var KEY_WAIT_MS = 10000
  // Headers of the ciphertext response that every decrypted response keeps,
  // so a decrypted page has a plain artifact's policy.
  var KEEP = ['content-security-policy', 'referrer-policy', 'x-content-type-options', 'permissions-policy',
    'cross-origin-resource-policy', 'x-robots-tag', 'cache-control', 'strict-transport-security']
  var state = null // {key: CryptoKey, artifact: string}
  var waiting = []
  var manifests = new Map() // version -> Promise of its manifest; numbers are never reused

  self.addEventListener('install', function (e) { e.waitUntil(self.skipWaiting()) })
  self.addEventListener('activate', function (e) { e.waitUntil(self.clients.claim()) })

  self.addEventListener('message', function (e) {
    var d = e.data, from = e.source
    if (!d || d.type !== 'redrop-e2e-key' || !from || from.type !== 'window' || from.frameType !== 'top-level') return
    if (typeof d.artifact !== 'string' || !(d.key instanceof CryptoKey)) return
    state = { key: d.key, artifact: d.artifact }
    manifests.clear()
    var ready = waiting
    waiting = []
    ready.forEach(function (resolve) { resolve(state) })
  })

  // key returns the key, asking the open viewer pages for it when this
  // worker was started without one.
  function key() {
    if (state) return Promise.resolve(state)
    return new Promise(function (resolve, reject) {
      var timer = setTimeout(function () {
        waiting = waiting.filter(function (w) { return w !== done })
        reject(problemError(503, 'This page needs its viewer. Open the link again.'))
      }, KEY_WAIT_MS)
      function done(s) { clearTimeout(timer); resolve(s) }
      waiting.push(done)
      self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (list) {
        list.forEach(function (c) { if (c.frameType === 'top-level') c.postMessage({ type: 'redrop-e2e-need-key' }) })
      })
    })
  }

  function problemError(status, message) {
    var e = new Error(message)
    e.status = status
    return e
  }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
    })
  }

  // problem is redrop's own small page for the frame: never artifact bytes.
  function problem(status, message) {
    var body = '<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">' +
      '<title>redrop</title><style>body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#333}' +
      '@media(prefers-color-scheme:dark){body{background:#111;color:#ddd}}</style><p>' + escapeHTML(message) + '</p>'
    return new Response(body, {
      status: status,
      headers: {
        'Content-Type': 'text/html; charset=utf-8',
        'Content-Security-Policy': "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'self'",
        'Cache-Control': 'no-store',
        'Referrer-Policy': 'no-referrer',
        'X-Content-Type-Options': 'nosniff',
      },
    })
  }

  // imagePage is redrop's page for a file artifact that is an image, as the
  // plain file page shows one: in an <img>, where an SVG's scripts don't
  // run. The <img> request (destination "image") gets the file itself.
  function imagePage(f, url) {
    var name = escapeHTML(f.path.slice(f.path.lastIndexOf('/') + 1))
    var body = '<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">' +
      '<title>' + name + '</title><style>html,body{margin:0;height:100%}body{display:grid;place-items:center;background:#f4f4f5}' +
      '@media(prefers-color-scheme:dark){body{background:#18181b}}img{max-width:100%;max-height:100vh}</style>' +
      '<img src="' + escapeHTML(url.pathname) + '" alt="' + name + '">'
    return new Response(body, {
      status: 200,
      headers: {
        'Content-Type': 'text/html; charset=utf-8',
        'Content-Security-Policy': "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'self'",
        'Cache-Control': 'no-store',
        'Referrer-Policy': 'no-referrer',
        'X-Content-Type-Options': 'nosniff',
      },
    })
  }

  function httpProblem(status) {
    if (status === 403) return problemError(403, 'Your access to this link has ended. Reload the page to open it again.')
    if (status === 429) return problemError(429, 'Too many requests. Wait a minute and reload the page.')
    return problemError(404, "This link doesn't exist, or it has expired.")
  }

  function objectURL(n, name) { return '/__redrop/v/' + n + '/' + name }

  function fetchObject(n, name) {
    return fetch(objectURL(n, name), { credentials: 'same-origin', cache: 'no-store', redirect: 'error' })
      .then(function (res) {
        if (!res.ok) throw httpProblem(res.status)
        return res
      })
  }

  function manifest(s, n) {
    var p = manifests.get(n)
    if (!p) {
      p = fetchObject(n, 'manifest')
        .then(function (res) { return res.arrayBuffer() })
        .then(function (buf) { return E.openBytes(new Uint8Array(buf), s.key, E.manifestBinding(s.artifact, n)) })
        .then(E.parseManifest)
      manifests.set(n, p)
      p.catch(function () { if (manifests.get(n) === p) manifests.delete(n) })
    }
    return p
  }

  function encodePath(p) { return p.split('/').map(encodeURIComponent).join('/') }

  // inline reports whether a file artifact's browser view shows the file;
  // anything else downloads.
  function inline(type) {
    return /^(image|audio|video|text)\//.test(type) || /^application\/(pdf|json|xml)/.test(type)
  }

  function headersFor(res, m, f, url) {
    var h = new Headers()
    KEEP.forEach(function (name) {
      var v = res.headers.get(name)
      if (v !== null) h.set(name, v)
    })
    var type = f.type, attach = url.searchParams.has('download')
    if (m.kind === 'file') {
      // A file artifact's page shows text as text, as the plain file page does.
      if (/^(text\/|application\/(json|xml))/.test(type)) type = 'text/plain; charset=utf-8'
      attach = attach || !inline(type)
    }
    h.set('Content-Type', type)
    var name = f.path.slice(f.path.lastIndexOf('/') + 1)
    h.set('Content-Disposition', (attach ? 'attachment' : 'inline') + "; filename*=UTF-8''" + encodeURIComponent(name))
    return h
  }

  function serve(req, url, n, rel) {
    var s, m
    return key().then(function (got) {
      s = got
      return manifest(s, n)
    }).then(function (got) {
      m = got
      var path
      try {
        path = decodeURIComponent(rel)
      } catch (e) {
        throw problemError(404, 'Not found')
      }
      var r = E.resolve(m, path)
      if (r.redirect) return Response.redirect(new URL(objectURL(n, encodePath(r.redirect)) + url.search, url).href, 301)
      if (!r.file) {
        throw problemError(404, r.listing
          ? 'This folder has no start page. File listings are not available for end-to-end encrypted links.'
          : 'Not found')
      }
      var f = r.file
      if (m.kind === 'file' && /^image\//.test(f.type) && req.mode === 'navigate' && !url.searchParams.has('download')) {
        return imagePage(f, url)
      }
      return fetchObject(n, f.object).then(function (res) {
        var headers = headersFor(res, m, f, url)
        var b = E.fileBinding(s.artifact, n, f.path)
        if (f.size <= E.BUFFER_LIMIT) {
          // Small enough to check completely before releasing a byte.
          return res.arrayBuffer().then(function (buf) {
            return E.openBytes(new Uint8Array(buf), s.key, b)
          }).then(function (plain) {
            headers.set('Content-Length', String(plain.length))
            return new Response(req.method === 'HEAD' ? null : plain, { status: 200, headers: headers })
          })
        }
        headers.set('Content-Length', String(f.size))
        return new Response(req.method === 'HEAD' ? null : E.openStream(res.body, s.key, b), { status: 200, headers: headers })
      })
    }).catch(function (err) {
      if (err && err.status) return problem(err.status, err.message)
      if (err && (err.code === 'auth' || err.code === 'format' || err.code === 'manifest')) {
        return problem(502, 'This content failed its integrity check: it was changed, or the link is incomplete. Nothing of it is shown.')
      }
      return problem(500, 'This content could not be opened. Reload the page to try again.')
    })
  }

  self.addEventListener('fetch', function (e) {
    var req = e.request, url = new URL(req.url)
    if (url.origin !== self.location.origin || (req.method !== 'GET' && req.method !== 'HEAD')) return
    // Top-level pages always go to the server: gates, views, the viewer.
    if (req.mode === 'navigate' && req.destination === 'document') return
    var m = VERSION_PATH.exec(url.pathname)
    if (m) {
      e.respondWith(serve(req, url, Number(m[1]), m[2]))
      return
    }
    if (url.pathname.indexOf('/__redrop/') === 0 || !e.clientId) return
    // A root-absolute subresource (/app.css) of a page inside version N
    // resolves in version N.
    e.respondWith(self.clients.get(e.clientId).then(function (c) {
      var cm = c && VERSION_PATH.exec(new URL(c.url).pathname)
      return cm ? serve(req, url, Number(cm[1]), url.pathname.slice(1)) : fetch(req)
    }))
  })
})()
