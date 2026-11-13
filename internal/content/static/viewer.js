// redrop viewer page and file page (DESIGN.md "Viewer frame"). Loaded from
// /__redrop/viewer.js; the pages carry no inline script. It adds local and
// relative times to the server-rendered info panel, a live expiry countdown
// while the panel is open, and the address bar that follows the frame.
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
  // The raw host reports every page the frame navigates to; this mirrors it
  // with replaceState (never pushState: the frame owns history entries).
  // One raw cookie serves every tab of an artifact version in a browser, so a
  // report is applied only around this frame's own load event.
  var frame = doc.getElementById('rd-frame')
  if (!frame || !window.EventSource || !frame.getAttribute('data-stream')) return
  var version = frame.getAttribute('data-version')
  var loadedAt = 0, pending = null
  function apply(path) {
    if (typeof path !== 'string' || path.charAt(0) !== '/' || path.charAt(1) === '/' || path.charAt(1) === '\\') return
    var u = new URL(path, location.origin)
    if (u.origin !== location.origin) return
    if (version && version !== '0') u.searchParams.set('redrop-version', version)
    var next = u.pathname + u.search
    if (next !== location.pathname + location.search) history.replaceState(history.state, '', next)
  }
  frame.addEventListener('load', function () {
    loadedAt = Date.now()
    if (pending && loadedAt - pending.at < 10000) apply(pending.path)
    pending = null
  })
  var es = new EventSource(frame.getAttribute('data-stream'))
  es.onmessage = function (e) {
    if (Date.now() - loadedAt < 3000) { apply(e.data); return }
    pending = { path: e.data, at: Date.now() }
  }
})()
