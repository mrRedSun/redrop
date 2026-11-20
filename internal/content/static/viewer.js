// redrop viewer page and file page (DESIGN.md "Viewer frame"). Loaded from
// /__redrop/viewer.js; the pages carry no inline script. It adds local and
// relative times to the server-rendered info panel, a live expiry countdown
// while the panel is open, keeps the address bar on the frame's page, sends
// the panel's feedback form (DESIGN.md "Viewer feedback"), and reloads the
// frame when the owner's watch publishes a version (DESIGN.md "Live reload").
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
  // barURL maps the frame's location to the address bar: version N's files
  // (/__redrop/v/N/path) show as path?redrop-version=N. It returns null for
  // another origin, for redrop's own pages, and for a location without a
  // path (the frame's initial about:blank).
  function barURL(loc) {
    if (loc.origin !== location.origin) return null
    var path = loc.pathname, search = loc.search
    if (path.charAt(0) !== '/') return null
    var m = OLD.exec(path)
    if (m) {
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
  function sync() {
    try {
      var next = barURL(frame.contentWindow.location)
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
  if (location.hash) frame.src = frame.getAttribute('src') + location.hash

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
    var es = new window.EventSource(row.getAttribute('data-src'))
    var pending = false, again = false, saved = null
    es.addEventListener('open', function () { row.hidden = false })
    // "end": live mode stopped or access ended. No more requests.
    es.addEventListener('end', function () { es.close(); row.hidden = true })
    // A refused or failed stream is closed for good (readyState 2);
    // a dropped one reconnects by itself.
    es.addEventListener('error', function () { if (es.readyState === 2) row.hidden = true })
    es.addEventListener('version', function (e) {
      var v = null
      try { v = JSON.parse(e.data) } catch (x) { return }
      if (!v || typeof v.version !== 'number' || v.version === shown) return
      shown = v.version
      panelShows(v)
      reload()
    })
    function reload() {
      if (pending) { again = true; return }
      try {
        var win = frame.contentWindow, loc = win.location
        // Another origin's page, or redrop's own (an older version's
        // files), is left alone.
        if (loc.origin !== location.origin || loc.pathname.indexOf('/__redrop/') === 0) return
        saved = { x: win.scrollX, y: win.scrollY }
        pending = true
        loc.reload()
      } catch (x) {
        pending = false
      }
    }
    frame.addEventListener('load', function () {
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
