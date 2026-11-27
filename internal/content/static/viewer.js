// redrop viewer page and file page (DESIGN.md "Viewer frame"). Loaded from
// /__redrop/viewer.js; the pages carry no inline script. It adds local and
// relative times to the server-rendered info panel, a live expiry countdown
// while the panel is open, keeps the address bar on the frame's page, sends
// the panel's feedback form (DESIGN.md "Viewer feedback"), and reloads the
// frame when the owner's watch publishes a version (DESIGN.md "Live reload").
// On an end-to-end encrypted link it adopts the intake CryptoKey and starts
// the decrypting worker (DESIGN.md "End-to-end encrypted links").
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
  function absolute(e) {
    e.textContent = new Date(e.getAttribute('data-abs')).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
  }
  each('[data-abs]', absolute)
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
  // replaceAddress is how the viewer changes its own address; an
  // end-to-end link's goes through its navigation guard (bootE2E).
  var replaceAddress = function (url) { history.replaceState(history.state, '', url) }
  function sync() {
    try {
      var next = addressFor(frame.contentWindow.location)
      if (next !== null && next !== location.pathname + location.search + location.hash) {
        replaceAddress(next)
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
  // The key never reaches this origin as text. The link points at the app
  // origin's intake page (https://<app>/e/<slug>#k=<key>), which imports it
  // as a non-extractable CryptoKey and hands that, through a helper window
  // on this origin, to this origin's IndexedDB as the intake key (see
  // e2e-intake.js and e2e-helper.js). The viewer adopts it for the artifact
  // once it matches the key check, and from then on reads it from there.
  // Anything key-like in this page's own address is a damaged link.
  //
  // Once the frame exists, the artifact's scripts share this window: the
  // navigation guard (guardNavigation) stops every change of this page's
  // address that the viewer didn't make, so no URL ever reaches it by a
  // fragment change.
  function bootE2E() {
    var E = window.RedropE2E
    var note = doc.getElementById('rd-e2e-msg')
    var guard = guardNavigation()
    if (guard) replaceAddress = guard.replace
    var hash = location.hash.slice(1)
    // Any fragment that could hold a key is a key attempt: a good key, or a
    // damaged one whose text must go nowhere (review 33 re-check B-2b').
    var parts = E ? E.splitFragment(hash) : { key: null, rest: '', damaged: true }
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
      else if (code === 'path') show('This link is damaged: its # was changed to %23, so its key went into the address. Nothing was opened. Ask for the link again and open it exactly as you get it.')
      else if (code === 'query') show('This link is damaged: its # was changed to ?, so its key went into the address. Nothing was opened. Ask for the link again and open it exactly as you get it.')
      else if (code === 'browser') show('This browser can\'t keep an end-to-end encrypted link\'s key from the page it opens (it lacks the Navigation API), so the page isn\'t opened. Use a current version of Chrome, Edge, Firefox or Safari.')
      else if (code === 'guard') show('The page was closed to keep its key safe: a link was opened in this tab, or the page changed the tab\'s address, while it was open. Open the link in a new tab.')
      else show('This page could not be opened. Reload to try again.')
    }
    // The viewer never changes the fragment with an event (replaceState
    // fires none), so a hashchange is the visitor's: a pasted full link or
    // an edited page. Start over with it.
    // Until the frame exists, that is safe: nothing but the viewer runs here.
    window.addEventListener('hashchange', function () { location.reload() })
    if (!E) {
      // The decryptor didn't load: nothing can open, and a key leaves the
      // address bar all the same.
      replaceAddress(location.pathname + marked(location.search))
      failed(coded('unavailable'))
      return
    }
    if (E.hasEncodedFragment(location.pathname)) {
      // The link's # became %23: its key is in the path (the server answers
      // these itself; this is the viewer's own refusal).
      replaceAddress('/' + marked(''))
      failed(coded('path'))
      return
    }
    if (E.queryKeyAttempt(location.search.slice(1))) {
      // The link's # became ?: its key is in the query, which the frame's
      // address would carry (review 33 re-check B-2b″). Refused before any
      // key, the fragment's too, is used.
      replaceAddress(location.pathname + marked(''))
      failed(coded('query'))
      return
    }
    if (parts.damaged) {
      // A key attempt that doesn't parse: no part of it goes anywhere, and
      // a key stored from an earlier visit doesn't open the page through it.
      replaceAddress(clean)
      failed(coded('key'))
      return
    }
    if (!guard) {
      // Without the guard, a page that opened could later see a full link
      // opened in this tab: refuse to open any (fail closed).
      replaceAddress(clean)
      failed(coded('browser'))
      return
    }
    if (parts.key) {
      // Key text on this origin: not a link redrop gives out (they point at
      // the app origin's intake page). It goes nowhere, not even into the
      // key store.
      replaceAddress(clean)
      failed(coded('key'))
      return
    }
    // The live viewer: always at its marked address before a frame exists.
    if (location.search !== marked(location.search)) replaceAddress(clean)
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
    // adopt takes the intake key the helper stored, if any, for this
    // artifact once it passes the key check; it is never kept otherwise. A
    // fresh hand-off comes first, before a key stored on an earlier visit.
    function adopt() {
      return E.keys.get(E.INTAKE_KEY, Date.now()).then(function (k) {
        if (!k) return null
        return E.keys.remove(E.INTAKE_KEY).then(function () {
          return E.keyCheck(k, e2e.artifact)
        }).then(function (check) {
          if (check !== e2e.check) throw coded('wrongkey')
          return E.keys.put(e2e.artifact, k, Date.now()).then(function () { return k })
        })
      })
    }
    adopt().then(function (k) {
      return k || E.keys.get(e2e.artifact, Date.now())
    }).then(function (k) {
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
      guard.arm(function () { failed(coded('guard')) })
      frame.src = target
    }).catch(failed)
  }

  // guardNavigation keeps every URL this page's address changes to away
  // from the artifact (review 33 re-check B-2a'). It runs before any frame
  // exists and captures everything it uses then, because the artifact,
  // sharing this window later, can replace any function or getter on it.
  // Its navigate listener is this window's first and a capturing one, so it
  // runs before any the artifact adds, capturing or not, in any engine
  // (review 33 re-check M5). Once armed (when the frame is created), every
  // same-document navigation of this page that the viewer didn't start (the
  // full link or a fragment opened in this tab, or the artifact rewriting
  // the address) is cancelled before the URL changes, hidden from later
  // listeners, and ends the artifact: every frame is removed, so none of
  // its code runs here again. The viewer's own address updates are known by
  // the exact URL they go to, not by a flag, and are hidden from later
  // listeners too, so no write can ride along inside one (m-6). It returns
  // null when the browser lacks the Navigation API; the viewer then opens
  // nothing.
  function guardNavigation() {
    var nav, apply, add, destination, sameDocument, urlOf, cancelable, stopNow, cancel, replaceState, stateOf, hist, findAll, removeNode, MakeURL, hrefOf
    try {
      var getter = function (proto, name) {
        var d = Object.getOwnPropertyDescriptor(proto, name)
        if (!d || typeof d.get !== 'function') throw new Error(name)
        return d.get
      }
      nav = window.navigation
      apply = Reflect.apply
      add = EventTarget.prototype.addEventListener
      destination = getter(NavigateEvent.prototype, 'destination')
      sameDocument = getter(NavigationDestination.prototype, 'sameDocument')
      urlOf = getter(NavigationDestination.prototype, 'url')
      MakeURL = URL
      hrefOf = getter(URL.prototype, 'href')
      cancelable = getter(Event.prototype, 'cancelable')
      stopNow = Event.prototype.stopImmediatePropagation
      cancel = Event.prototype.preventDefault
      replaceState = History.prototype.replaceState
      stateOf = getter(History.prototype, 'state')
      hist = window.history
      findAll = Document.prototype.querySelectorAll
      removeNode = Element.prototype.remove
      if (!nav || typeof apply !== 'function' || typeof stopNow !== 'function' || typeof cancel !== 'function' ||
        typeof replaceState !== 'function' || typeof findAll !== 'function' || typeof removeNode !== 'function' ||
        typeof MakeURL !== 'function') return null
    } catch (e) {
      return null
    }
    var expected = null, armed = false, tripped = null
    apply(add, nav, ['navigate', function (e) {
      var dest = apply(destination, e, [])
      var same = apply(sameDocument, dest, [])
      if (expected !== null && same && apply(urlOf, dest, []) === expected) {
        apply(stopNow, e, [])
        return
      }
      if (!armed || !same) return
      apply(stopNow, e, [])
      if (apply(cancelable, e, [])) apply(cancel, e, [])
      var frames = apply(findAll, doc, ['iframe, frame, object, embed'])
      for (var i = 0, f; (f = frames[i]); i++) apply(removeNode, f, [])
      if (tripped) {
        var then = tripped
        tripped = null
        then()
      }
    }, true])
    return {
      replace: function (url) {
        expected = apply(hrefOf, new MakeURL(url, location.href), [])
        try {
          apply(replaceState, hist, [apply(stateOf, hist, []), '', url])
        } finally {
          expected = null
        }
      },
      arm: function (then) {
        armed = true
        tripped = then
      },
    }
  }

  // ---- live reload ----
  // The page carries #rd-live only while the owner's watch holds a lease.
  // On a new current version the frame's own page is reloaded: the same
  // path, query and fragment, and no history entry. Nothing is written to
  // the artifact's document; its scroll position is read before the reload
  // and, if the browser left the page at the top, restored after it.
  var liveRow = doc.getElementById('rd-live')
  if (liveRow && window.EventSource) live(liveRow)

  function live(row) {
    var shown = Number(row.getAttribute('data-version'))
    var es = null
    var pending = false, again = false, saved = null, reloadTimer = 0
    function connect() {
      var stream = new window.EventSource(row.getAttribute('data-src'))
      es = stream
      stream.addEventListener('open', function () { if (es === stream) row.hidden = false })
      // Only an explicit shutdown end permits one delayed reconnect. Errors
      // must not let EventSource retry and spend the network's miss budget.
      stream.addEventListener('end', function (e) {
        if (es !== stream) return
        stream.close()
        es = null
        row.hidden = true
        var end = null
        try { end = JSON.parse(e.data) } catch { return }
        if (end && end.reconnect === true) {
          setTimeout(connect, 25000)
        }
      })
      stream.addEventListener('error', function () {
        stream.close()
        if (es === stream) { es = null; row.hidden = true }
      })
      stream.addEventListener('version', function (e) {
        if (es !== stream) return
        var v = null
        try { v = JSON.parse(e.data) } catch (x) { return }
        if (!v || !Number.isSafeInteger(v.version) || v.version < 1 || v.version === shown) return
        if (typeof v.published !== 'string' || !Number.isFinite(Date.parse(v.published))) return
        if (typeof v.size !== 'string' || !Number.isSafeInteger(v.files) || v.files < 0) return
        shown = v.version
        panelShows(v)
        reload()
      })
    }
    connect()
    function reload() {
      if (pending) { again = true; return }
      try {
        var win = frame.contentWindow, loc = win.location
        // Another origin's page, or redrop's own (an older version's
        // files), is left alone.
        if (loc.origin !== location.origin || loc.pathname.indexOf('/__redrop/') === 0) return
        saved = { x: win.scrollX, y: win.scrollY }
        pending = true
        // A cancelled beforeunload prompt produces no load event. Forget the
        // attempt after 30 s so a later version can reload, without retrying it.
        reloadTimer = setTimeout(function () {
          pending = false; saved = null; again = false; reloadTimer = 0
        }, 30000)
        loc.reload()
      } catch (x) {
        clearTimeout(reloadTimer)
        pending = false; saved = null; again = false; reloadTimer = 0
      }
    }
    frame.addEventListener('load', function () {
      clearTimeout(reloadTimer)
      reloadTimer = 0
      if (!pending) return
      pending = false
      var at = saved
      saved = null
      try {
        var win = frame.contentWindow
        if (at && (at.x || at.y) && !win.scrollX && !win.scrollY) win.scrollTo(at.x, at.y)
      } catch (x) {
        // Another origin by now.
      }
      // A version that arrived during the reload may not be in it.
      if (again) { again = false; reload() }
    })
  }

  // panelShows makes the panel describe version v, now current and shown.
  function panelShows(v) {
    var line = doc.getElementById('rd-shown')
    if (line) {
      var when = doc.createElement('span'), abs = doc.createElement('small')
      when.setAttribute('data-rel', v.published)
      abs.setAttribute('data-abs', v.published)
      line.textContent = 'Version ' + v.version + ', published '
      line.appendChild(when)
      line.appendChild(abs)
      absolute(abs)
    }
    var size = doc.getElementById('rd-size')
    if (size) size.textContent = v.size + ' in ' + v.files + ' file' + (v.files === 1 ? '' : 's')
    // Comments are filed against the version on screen.
    var fb = doc.getElementById('rd-fb')
    if (fb) {
      fb.setAttribute('data-version', String(v.version))
      fb.setAttribute('data-current', String(v.version))
    }
    var list = doc.querySelector('.rd-revs'), cur = null
    if (list) {
      each('.rd-revs > li', function (li) {
        li.removeAttribute('aria-current')
        Array.prototype.forEach.call(li.querySelectorAll('.rd-tag'), function (t) { li.removeChild(t) })
        if (li.getAttribute('data-rev') === String(v.version)) cur = li
      })
      if (!cur) {
        cur = doc.createElement('li')
        cur.setAttribute('data-rev', String(v.version))
        cur.appendChild(doc.createTextNode('Version ' + v.version))
        cur.appendChild(doc.createTextNode(' · '))
        var t = doc.createElement('span')
        t.setAttribute('data-rel', v.published)
        cur.appendChild(t)
        list.insertBefore(cur, list.firstChild)
      }
      var tag = doc.createElement('span')
      tag.className = 'rd-tag'
      tag.textContent = 'current'
      cur.insertBefore(tag, cur.firstChild.nextSibling)
      cur.insertBefore(doc.createTextNode(' '), tag)
      cur.setAttribute('aria-current', 'page')
    }
    tick()
  }
})()
