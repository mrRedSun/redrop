// redrop viewer page and file page (DESIGN.md "Viewer frame"). Loaded from
// /__redrop/viewer.js; the pages carry no inline script. It adds local and
// relative times to the server-rendered info panel, a live expiry countdown
// while the panel is open, and keeps the address bar on the frame's page.
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

  // ---- the address bar follows the frame ----
  // The frame is on this origin, so its location is read directly after
  // every load and every same-document URL change inside it (a fragment,
  // history.pushState or replaceState, Back/Forward). The artifact's bytes
  // stay untouched and nothing in its window is written or patched: the
  // viewer only reads its location and listens for events.
  // replaceState, never pushState: the frame owns the history entries.
  var frame = doc.getElementById('rd-frame')
  if (!frame) return
  var OLD = /^\/__redrop\/v\/([1-9][0-9]*)(\/.*)$/
  var POLL_MS = 250
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
})()
