// redrop viewer page and file page (DESIGN.md "Viewer frame"). Loaded from
// /__redrop/viewer.js; the pages carry no inline script. It adds local and
// relative times to the server-rendered info panel, a live expiry countdown
// while the panel is open, keeps the address bar on the frame's page, and
// sends the panel's feedback form (DESIGN.md "Viewer feedback"). On an
// end-to-end encrypted link it also takes the key from the fragment and
// starts the decrypting worker (DESIGN.md "End-to-end encrypted links").
(function () {
  'use strict'
  var doc = document

  // ---- times ----
  var rtf = window.Intl && Intl.RelativeTimeFormat ? new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' }) : null
  function rel(t) {
    var s = (t - Date.now()) / 1000, a = Math.abs(s)
    var u = a < 60 ? ['second', 1] : a < 3600 ? ['minute', 60] : a < 86400 ? ['hour', 3600] : ['day', 86400]
    return rtf ? rtf.format(Math.round(s / u[1]), u[0]) : t.toLocaleString()
  }
  function each(sel, fn) { Array.prototype.forEach.call(doc.querySelectorAll(sel), fn) }
  function tick() {
    each('[data-rel]', function (e) { e.textContent = rel(new Date(e.getAttribute('data-rel'))) })
    each('[data-expires]', function (e) {
      var t = new Date(e.getAttribute('data-expires'))
      e.textContent = (t > Date.now() ? 'Expires ' : 'Expired ') + rel(t)
    })
  }
  each('[data-abs]', function (e) {
    e.textContent = new Date(e.getAttribute('data-abs')).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
  })
  tick()
  var panel = doc.getElementById('rd-info'), timer = 0
  if (panel) {
    panel.addEventListener('toggle', function (e) {
      clearInterval(timer)
      if (e.newState === 'open') { tick(); timer = setInterval(tick, 1000) }
    })
  }

  var frame = doc.getElementById('rd-frame')
  var OLD = /^\/__redrop\/v\/([1-9][0-9]*)(\/.*)$/
  // An end-to-end encrypted link's frame always shows a version path, the
  // current version's too (data-current).
  var e2e = frame && frame.getAttribute('data-e2e') !== null ? {
    artifact: frame.getAttribute('data-artifact'),
    version: frame.getAttribute('data-version'),
    current: frame.getAttribute('data-current'),
    check: frame.getAttribute('data-key-check'),
  } : null
  // barURL maps the frame's location to the address bar: version N's files
  // (/__redrop/v/N/path) show as path?redrop-version=N. It returns null for
  // another origin, for redrop's own pages, and for a location without a
  // path (the frame's initial about:blank).
  function barURL(loc) {
    if (loc.origin !== location.origin) return null
    var path = loc.pathname, search = loc.search
    if (path.charAt(0) !== '/') return null
    var m = OLD.exec(path)
    if (m && e2e && m[1] === e2e.current) {
      path = m[2]
    } else if (m) {
      var q = new URLSearchParams(search)
      q.set('redrop-version', m[1])
      path = m[2]
      search = '?' + q.toString()
    } else if (path.indexOf('/__redrop/') === 0) {
      return null
    }
    return path + search + loc.hash
  }

  // ---- feedback ----
  // The form is in the panel, outside the artifact. The artifact is only
  // read: its selection (for a quote) and its location (for the page and
  // version). Nothing is written to its document or window, and no listener
  // is added there: the selection is read when the panel opens.
  var form = doc.getElementById('rd-fb')
  if (form) feedback(form)

  function feedback(form) {
    var MAX_EXACT = 500, CONTEXT = 64
    var body = doc.getElementById('rd-fb-body'), name = doc.getElementById('rd-fb-name')
    var send = doc.getElementById('rd-fb-send'), status = doc.getElementById('rd-fb-status')
    var box = doc.getElementById('rd-fb-quote'), boxText = doc.getElementById('rd-fb-quote-text')
    var quote = null
    function say(msg) { status.textContent = msg }
    function setQuote(q) {
      quote = q
      boxText.textContent = q ? q.exact : ''
      box.hidden = !q
    }
    // quoteOf returns a W3C text quote of win's selection, or null. Code
    // points, as the server counts them; a long selection is shortened and
    // its suffix taken after the shortened text, so the quote still anchors.
    // Strings are cut in UTF-16 units before they become code point arrays,
    // so the arrays stay small however long the page or selection is: n code
    // points never need more than 2n units.
    function quoteOf(win, skip) {
      var sel = win.getSelection()
      if (!sel || sel.isCollapsed || !sel.rangeCount) return null
      var range = sel.getRangeAt(0)
      if (skip && skip.contains(range.commonAncestorContainer)) return null
      var s = range.toString()
      if (!/\S/.test(s)) return null
      var head = Array.from(s.slice(0, 2 * (MAX_EXACT + CONTEXT)))
      var d = range.startContainer.ownerDocument || range.startContainer
      var root = d.body || d.documentElement
      var before = d.createRange()
      before.setStart(root, 0)
      before.setEnd(range.startContainer, range.startOffset)
      var rest = head.slice(MAX_EXACT)
      if (rest.length < CONTEXT) {
        var after = d.createRange()
        after.setStart(range.endContainer, range.endOffset)
        after.setEnd(root, root.childNodes.length)
        rest = rest.concat(Array.from(after.toString().slice(0, 2 * CONTEXT)))
      }
      return {
        exact: head.slice(0, MAX_EXACT).join(''),
        prefix: Array.from(before.toString().slice(-2 * CONTEXT)).slice(-CONTEXT).join(''),
        suffix: rest.slice(0, CONTEXT).join(''),
      }
    }
    function capture() {
      var q = null
      try {
        q = frame ? quoteOf(frame.contentWindow, null) : quoteOf(window, panel)
      } catch (e) {
        // Another origin, or a selection outside the document's body.
      }
      setQuote(q)
    }
    // where is the page and version on screen: the frame's page as the
    // address bar shows it, else (no frame, or another origin) the address
    // bar and this page's version.
    function where() {
      var current = Number(form.getAttribute('data-current'))
      if (frame) {
        try {
          var loc = frame.contentWindow.location, page = barURL(loc)
          if (page !== null) {
            var m = OLD.exec(loc.pathname)
            return { page: page, version: m ? Number(m[1]) : current }
          }
        } catch (e) {
          // Another origin: fall back to the address bar.
        }
      }
      return { page: location.pathname + location.search + location.hash, version: Number(form.getAttribute('data-version')) }
    }
    if (panel) {
      panel.addEventListener('toggle', function (e) {
        if (e.newState === 'open') capture()
      })
    }
    doc.getElementById('rd-fb-unquote').addEventListener('click', function () { setQuote(null) })
    form.addEventListener('submit', function (e) {
      e.preventDefault()
      if (!body.value.trim()) { say('Write a comment first.'); return }
      var w = where()
      var msg = { body: body.value, name: name.value, version: w.version, page: w.page }
      if (quote) msg.quote = quote
      send.disabled = true
      say('Sending…')
      fetch('/__redrop/feedback', {
        method: 'POST', credentials: 'same-origin', cache: 'no-store',
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(msg),
      }).then(function (res) {
        if (res.status === 201) {
          body.value = ''
          setQuote(null)
          say('Thanks, your feedback was sent.')
          return
        }
        return res.json().then(function (j) {
          say(j && typeof j.message === 'string' ? j.message : 'Feedback could not be sent.')
        }, function () { say('Feedback is not available on this page right now.') })
      }, function () {
        say('Could not reach the server. Check your connection and try again.')
      }).then(function () { send.disabled = false })
    })
    form.hidden = false
  }

  // ---- the address bar follows the frame ----
  // The frame is on this origin, so its location is read directly after
  // every load and every same-document URL change inside it (a fragment,
  // history.pushState or replaceState, Back/Forward). The artifact's bytes
  // stay untouched and nothing in its window is written or patched: the
  // viewer only reads its location and listens for events.
  // replaceState, never pushState: the frame owns the history entries.
  if (!frame) return
  var POLL_MS = 250
  var E2E_MARK = 'redrop-e2e'
  // marked is search with the live viewer's E2E_MARK query.
  function marked(search) {
    var q = new URLSearchParams(search)
    q.set(E2E_MARK, '1')
    return '?' + q.toString()
  }
  // addressFor is the address bar for the frame's location. An end-to-end
  // encrypted link keeps the page in the fragment (/#/docs/a.html#part),
  // so a reload sends the server only "/" and the paths stay private, and
  // always keeps the E2E_MARK query: a full link (/#k=…) then differs from
  // the live viewer's address by more than its fragment, so opening it in
  // this tab loads a new document instead of handing the key to this one
  // (review 33 re-check B-2a).
  function addressFor(loc) {
    if (!e2e) return barURL(loc)
    var m = loc.origin === location.origin && OLD.exec(loc.pathname)
    if (!m) return null
    return '/?' + E2E_MARK + '=1' + (m[1] !== e2e.current ? '&redrop-version=' + m[1] : '') + '#' + m[2] + loc.search + loc.hash
  }
  function sync() {
    try {
      var next = addressFor(frame.contentWindow.location)
      if (next !== null && next !== location.pathname + location.search + location.hash) {
        history.replaceState(history.state, '', next)
      }
    } catch (e) {
      // The frame navigated to another origin: leave the address bar alone.
    }
  }

  // follow watches the frame's current document for URL changes that load
  // no page. With the Navigation API, its currententrychange event covers
  // every one; without it, pushState and replaceState fire no event, so the
  // frame's address is polled while this page is visible. Each document has
  // a new window, so this runs again after every load; stop undoes it, on
  // the next load and while the page is hidden.
  var unfollow = null
  function stop() {
    if (unfollow) unfollow()
    unfollow = null
  }
  function follow() {
    stop()
    sync()
    if (doc.visibilityState !== 'visible') return
    try {
      var win = frame.contentWindow, nav = win.navigation
      if (nav && typeof nav.addEventListener === 'function') {
        nav.addEventListener('currententrychange', sync)
        unfollow = function () {
          try { nav.removeEventListener('currententrychange', sync) } catch (e) { /* gone */ }
        }
        return
      }
      win.addEventListener('hashchange', sync)
      win.addEventListener('popstate', sync)
      var last = win.location.href
      var poll = setInterval(function () {
        try {
          var href = frame.contentWindow.location.href
          if (href !== last) { last = href; sync() }
        } catch (e) {
          // Another origin, until the next load.
        }
      }, POLL_MS)
      unfollow = function () {
        clearInterval(poll)
        // After a load, win already points at the new document (the old
        // window and its listeners are gone); this matters when hidden.
        try {
          win.removeEventListener('hashchange', sync)
          win.removeEventListener('popstate', sync)
        } catch (e) { /* gone */ }
      }
    } catch (e) {
      // Another origin: nothing to follow until the next load.
    }
  }
  frame.addEventListener('load', follow)
  doc.addEventListener('visibilitychange', function () {
    if (doc.visibilityState === 'visible') follow()
    else stop()
  })
  // The server never sees the fragment: hand it to the frame.
  if (e2e) bootE2E()
  else if (location.hash) frame.src = frame.getAttribute('src') + location.hash

  // ---- end-to-end encrypted links ----
  // The key comes from the link's fragment (#k=<key>, optionally followed
  // by & and the page or the artifact's own fragment). The artifact runs
  // on this origin and could read anything this page keeps, so the key's
  // text is never kept: it is imported as a non-extractable CryptoKey,
  // checked, and stored as that in IndexedDB; the fragment leaves the
  // address bar; and the page reloads, so its navigation records (the
  // Performance and Navigation APIs) never held the key either. Only that
  // reloaded page, which reads the CryptoKey back, creates the frame.
  function bootE2E() {
    var E = window.RedropE2E
    var note = doc.getElementById('rd-e2e-msg')
    var hash = location.hash.slice(1)
    // Any fragment that starts with k= is a key attempt: a good key, or a
    // damaged one whose text must go nowhere (review 33 re-check B-2b).
    var parts = E ? E.splitFragment(hash) : { key: null, rest: hash.indexOf('k=') === 0 ? '' : hash, damaged: false }
    var rest = parts.rest, key = null
    // The address without the key, at the live viewer's marked URL.
    var clean = location.pathname + marked(location.search) + (rest ? '#' + rest : '')
    var secure = window.isSecureContext && window.crypto && crypto.subtle && navigator.serviceWorker
    function show(text) {
      note.textContent = text
      note.hidden = false
    }
    function coded(code, status) {
      var e = new Error(code)
      e.code = code
      e.status = status
      return e
    }
    function failed(err) {
      var code = err && err.code
      if (code === 'missing') show('This page is end-to-end encrypted, and the key is missing from the link. Open the complete link you were given, including the part after #.')
      else if (code === 'wrongkey') show('This link\'s key doesn\'t open this page. Check that you have the complete link, including the part after #.')
      else if (code === 'key') show('This link\'s key is damaged. Open the complete link you were given, including the part after #.')
      else if (code === 'storage') show('This browser doesn\'t let the viewer keep the key while it opens the page (some private windows block site storage), so it can\'t be opened here.')
      else if (code === 'auth' || code === 'format' || code === 'manifest') show('This page failed its integrity check: its content was changed, or it belongs to another link. Nothing of it is shown.')
      else if (code === 'http' && err.status === 403) show('Your access to this link has ended. Reload the page to open it again.')
      else if (code === 'http' && err.status === 404) show('This link doesn\'t exist, or it has expired.')
      else if (code === 'worker') show('Your browser could not start the decryption worker. Allow this site to use service workers (some private windows block them) and reload.')
      else show('This page could not be opened. Reload to try again.')
    }
    // The viewer never changes the fragment with an event (replaceState
    // fires none), so a hashchange is the visitor's: a pasted full link or
    // an edited page. Start over with it.
    window.addEventListener('hashchange', function () { location.reload() })
    if (!E) {
      // The decryptor didn't load: nothing can open, and a key leaves the
      // address bar all the same.
      history.replaceState(history.state, '', clean)
      failed(coded('unavailable'))
      return
    }
    if (parts.damaged) {
      // A key attempt that doesn't parse: no part of it goes anywhere, and
      // a key stored from an earlier visit doesn't open the page through it.
      history.replaceState(history.state, '', clean)
      failed(coded('key'))
      return
    }
    if (parts.key) {
      // The link's key: off the address bar at once, then kept as a
      // CryptoKey, then a reload without it.
      var text = parts.key
      history.replaceState(history.state, '', clean)
      if (!secure) {
        show('This page is end-to-end encrypted. Browsers decrypt only on secure (HTTPS) pages that may use service workers, so it can\'t be opened here.')
        return
      }
      E.importKey(text).then(function (k) {
        return E.keyCheck(k, e2e.artifact).then(function (check) {
          if (check !== e2e.check) throw coded('wrongkey')
          return E.keys.put(e2e.artifact, k, Date.now())
        })
      }).then(function () { location.reload() }, failed)
      return
    }
    // The live viewer: always at its marked address before a frame exists.
    if (location.search !== marked(location.search)) history.replaceState(history.state, '', clean)
    // The frame's first page: a page in the fragment, else this path with
    // the artifact's own fragment.
    var target = rest.charAt(0) === '/'
      ? '/__redrop/v/' + e2e.version + rest
      : frame.getAttribute('data-src') + (rest ? '#' + rest : '')
    if (!secure) {
      show('This page is end-to-end encrypted. Browsers decrypt only on secure (HTTPS) pages that may use service workers, so it can\'t be opened here.')
      return
    }
    function object(name) {
      return fetch('/__redrop/v/' + e2e.version + '/' + name, { credentials: 'same-origin', cache: 'no-store' }).then(function (res) {
        if (!res.ok) throw coded('http', res.status)
        return res.arrayBuffer()
      }).then(function (buf) { return new Uint8Array(buf) })
    }
    function sendKey(to) {
      to.postMessage({ type: 'redrop-e2e-key', key: key, artifact: e2e.artifact })
    }
    function activeWorker(reg) {
      return new Promise(function (resolve, reject) {
        var w = reg.installing || reg.waiting
        if (!w) {
          if (reg.active) resolve(reg.active)
          else reject(coded('worker'))
          return
        }
        w.addEventListener('statechange', function () {
          if (w.state === 'activated') resolve(w)
          else if (w.state === 'redundant') {
            if (reg.active) resolve(reg.active)
            else reject(coded('worker'))
          }
        })
      })
    }
    function startWorker() {
      // A worker that was stopped asks for the key again.
      navigator.serviceWorker.addEventListener('message', function (e) {
        if (e.data && e.data.type === 'redrop-e2e-need-key' && e.source && key) sendKey(e.source)
      })
      navigator.serviceWorker.startMessages()
      return navigator.serviceWorker.register('/__redrop/e2e-sw.js', { scope: '/__redrop/v/', updateViaCache: 'none' })
        .catch(function () { throw coded('worker') })
        .then(activeWorker)
    }
    function offerDownload(m) {
      var button = doc.getElementById('rd-e2e-download')
      if (!button || m.kind !== 'file') return
      var f = m.byPath[m.entry]
      button.addEventListener('click', function () {
        button.disabled = true
        object(f.object).then(function (bytes) {
          return E.openBytes(bytes, key, E.fileBinding(e2e.artifact, Number(e2e.version), f.path))
        }).then(function (plain) {
          var url = URL.createObjectURL(new Blob([plain], { type: f.type }))
          var a = doc.createElement('a')
          a.href = url
          a.download = f.path.slice(f.path.lastIndexOf('/') + 1)
          doc.body.appendChild(a)
          a.click()
          a.remove()
          setTimeout(function () { URL.revokeObjectURL(url) }, 60000)
        }).catch(function () {
          button.textContent = 'Download failed. Reload and try again.'
        }).then(function () { button.disabled = false })
      })
      button.hidden = false
    }
    E.keys.get(e2e.artifact, Date.now()).then(function (k) {
      if (!k) throw coded('missing')
      key = k
      return E.keyCheck(k, e2e.artifact)
    }).then(function (check) {
      if (check !== e2e.check) {
        E.keys.remove(e2e.artifact)
        throw coded('wrongkey')
      }
      // Decrypting the manifest checks the content before anything is framed.
      return object('manifest')
    }).then(function (bytes) {
      return E.openBytes(bytes, key, E.manifestBinding(e2e.artifact, Number(e2e.version)))
    }).then(E.parseManifest).then(function (m) {
      if (m.title) {
        if (!doc.title) doc.title = m.title
        if (!frame.title) frame.title = m.title
        var heading = panel && panel.querySelector('h2')
        if (heading && !heading.textContent) heading.textContent = m.title
      }
      offerDownload(m)
      return startWorker()
    }).then(function (worker) {
      sendKey(worker)
      frame.src = target
    }).catch(failed)
  }
})()
