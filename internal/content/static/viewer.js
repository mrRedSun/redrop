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
  // every load, hash change and history step inside it. Nothing is added to
  // the artifact or patched in its window: the viewer only reads and listens.
  // replaceState, never pushState: the frame owns the history entries.
  var frame = doc.getElementById('rd-frame')
  if (!frame) return
  var OLD = /^\/__redrop\/v\/([1-9][0-9]*)(\/.*)$/
  // barURL maps the frame's location to the address bar: version N's files
  // (/__redrop/v/N/path) show as path?redrop-version=N. It returns null for
  // another origin and for redrop's own pages.
  function barURL(loc) {
    if (loc.origin !== location.origin) return null
    var path = loc.pathname, search = loc.search, m = OLD.exec(path)
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
  frame.addEventListener('load', function () {
    sync()
    try {
      // A new document has a new window: listen again on every load.
      frame.contentWindow.addEventListener('hashchange', sync)
      frame.contentWindow.addEventListener('popstate', sync)
    } catch (e) {
      // another origin
    }
  })
  // The server never sees the fragment: hand it to the frame.
  if (location.hash) frame.src = frame.getAttribute('src') + location.hash
})()
