function (d) {
  // redrop info button. Injected by the content server into top-level HTML
  // pages (see info.go and DESIGN.md "Info button"). The server calls this
  // function expression with the page's viewer info; it renders inside a
  // closed shadow root, makes no network requests, and leaves one global:
  // the __redropInfo guard against double injection.
  var w = window, doc = document
  if (w.top !== w || w.__redropInfo) return
  try {
    Object.defineProperty(w, '__redropInfo', { value: true })
    var host = doc.createElement('redrop-info')
    var root = host.attachShadow({ mode: 'closed' })
    var css = ':host{all:initial!important;display:block!important;position:fixed!important;z-index:2147483647!important;' +
      'right:max(12px,env(safe-area-inset-right))!important;bottom:max(12px,env(safe-area-inset-bottom))!important;' +
      '--bg:#fff;--fg:#171717;--mut:#666;--bd:#e5e5e5}' +
      '@media (prefers-color-scheme:dark){:host{--bg:#1f1f1f;--fg:#f5f5f5;--mut:#a3a3a3;--bd:#3a3a3a}}' +
      '.b{box-sizing:border-box;display:grid;place-items:center;width:32px;height:32px;margin:0;padding:0;' +
      'border:1px solid var(--bd);border-radius:50%;background:var(--bg);color:var(--fg);' +
      'font:italic 600 16px/1 Georgia,"Times New Roman",serif;cursor:pointer;opacity:.75;box-shadow:0 1px 3px rgba(0,0,0,.18)}' +
      '.b:hover,.b[aria-expanded=true]{opacity:1}' +
      '.b:focus-visible{outline:2px solid #2563eb;outline-offset:2px;opacity:1}' +
      '@media (pointer:coarse){.b{width:40px;height:40px}}' +
      '.p{position:absolute;right:0;bottom:calc(100% + 8px);box-sizing:border-box;width:max-content;' +
      'max-width:min(300px,calc(100vw - 24px));padding:12px 14px;border:1px solid var(--bd);border-radius:10px;' +
      'background:var(--bg);color:var(--fg);font:13px/1.45 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;' +
      'text-align:left;box-shadow:0 6px 24px rgba(0,0,0,.18)}' +
      '.p[hidden]{display:none}' +
      'h2{margin:0 0 6px;font-size:14px;font-weight:600;overflow-wrap:anywhere}' +
      'ul{margin:0;padding:0;list-style:none}li{margin:6px 0}' +
      'small{display:block;color:var(--mut);font-size:12px}' +
      '.f{display:flex;align-items:center;gap:6px;margin-top:10px;padding-top:8px;border-top:1px solid var(--bd);color:var(--mut);font-size:12px}' +
      '.f img{width:16px;height:16px}' +
      '@media (prefers-reduced-motion:no-preference){.p{animation:r .12s ease-out}@keyframes r{from{opacity:0;transform:translateY(4px)}}}'
    var sheet = null
    if ('adoptedStyleSheets' in root) {
      try { sheet = new CSSStyleSheet(); sheet.replaceSync(css); root.adoptedStyleSheets = [sheet] } catch (e) { sheet = null }
    }
    if (!sheet) el('style', '', css, root)

    var btn = el('button', 'b', 'i', root)
    btn.type = 'button'
    attr(btn, { 'aria-label': 'About this shared page', 'aria-expanded': 'false', 'aria-controls': 'p' })
    var panel = el('div', 'p', null, root)
    panel.id = 'p'
    panel.hidden = true
    attr(panel, { role: 'dialog', 'aria-label': 'About this shared page' })
    el('h2', '', d.title || 'Untitled', panel)
    var list = el('ul', '', null, panel)
    var published = el('li', '', null, list)
    var expiry = el('li', '', null, list)
    if (d.max_views === 1) el('li', '', 'One-time link: this was the only view.', list)
    else if (d.max_views > 1) el('li', '', d.views_left + ' of ' + d.max_views + ' views left', list)
    if (d.password) el('li', '', 'Password-protected', list)
    el('li', '', size(d.size) + ' in ' + d.files + (d.files === 1 ? ' file' : ' files'), list)
    var foot = el('div', 'f', null, panel)
    var mark = el('img', '', null, foot)
    mark.alt = ''
    mark.src = d.mark
    el('span', '', 'Shared with redrop', foot)

    var timer = 0
    var tick = function () {
      var pub = new Date(d.published)
      line(published, 'Version ' + d.version + ', published ' + rel(pub), abs(pub))
      if (!d.expires) return line(expiry, 'Never expires')
      var exp = new Date(d.expires)
      line(expiry, (exp > Date.now() ? 'Expires ' : 'Expired ') + rel(exp), abs(exp))
    }
    var outside = function (e) { if (e.composedPath().indexOf(host) < 0) toggle(false) }
    var key = function (e) {
      if (e.key !== 'Escape') return
      var inside = !!root.activeElement
      toggle(false)
      if (inside) btn.focus()
    }
    var toggle = function (open) {
      if (open === !panel.hidden) return
      panel.hidden = !open
      btn.setAttribute('aria-expanded', String(open))
      var m = open ? 'addEventListener' : 'removeEventListener'
      doc[m]('pointerdown', outside, true)
      doc[m]('keydown', key, true)
      clearInterval(timer)
      if (open) { tick(); timer = setInterval(tick, 1000) }
    }
    tick()
    btn.addEventListener('click', function () { toggle(panel.hidden) })
    ;(doc.body || doc.documentElement).appendChild(host)
  } catch (e) {
    // A page that forbids this (CSP, Trusted Types, frozen DOM) simply has no button.
  }

  function el(tag, cls, text, parent) {
    var e = doc.createElement(tag)
    if (cls) e.className = cls
    if (text != null) e.textContent = text
    parent.appendChild(e)
    return e
  }
  function attr(e, map) { for (var k in map) e.setAttribute(k, map[k]) }
  function line(li, text, detail) {
    li.textContent = text
    if (detail) el('small', '', detail, li)
  }
  function rel(t) {
    var s = (t - Date.now()) / 1000, a = Math.abs(s)
    var u = a < 60 ? ['second', 1] : a < 3600 ? ['minute', 60] : a < 86400 ? ['hour', 3600] : ['day', 86400]
    return new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' }).format(Math.round(s / u[1]), u[0])
  }
  function abs(t) { return t.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' }) }
  function size(n) {
    if (n < 1000) return n + ' B'
    var i = -1
    do { n /= 1000; i++ } while (n >= 1000 && i < 4)
    return n.toFixed(1) + ' ' + 'kMGTP'[i] + 'B'
  }
}
