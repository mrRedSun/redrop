// redrop's key intake helper, on an artifact's origin (DESIGN.md
// "End-to-end encrypted links", "Where the key goes in the browser").
// Served as /__redrop/e2e-intake.js after the decryptor (e2e.js), on the
// page /__redrop/e2e-intake, which only the app origin's intake page opens.
// It takes one non-extractable CryptoKey, from the window that opened it and
// only if that window is on the app origin, stores it as this origin's
// intake key (the viewer adopts it once it matches the artifact's key
// check), answers, and closes. It never sees the key's text.
;(function () {
  'use strict'
  var E = window.RedropE2E
  var page = document.getElementById('rd-e2e-intake')
  var msg = document.getElementById('rd-e2e-intake-msg')
  var app = page.getAttribute('data-app')
  var opener = window.opener
  function say(text) { msg.textContent = text }
  if (!E || !opener) {
    say('This window works only when an encrypted link opens it. Open the complete link you were given.')
    return
  }
  function reply(type) {
    try { opener.postMessage({ type: type }, app) } catch (e) { /* the opener went away */ }
  }
  var done = false
  window.addEventListener('message', function (e) {
    if (done || e.source !== opener || e.origin !== app || !e.data || e.data.type !== 'redrop-e2e-key') return
    var k = e.data.key
    if (!(k instanceof CryptoKey) || k.extractable || k.type !== 'secret' || !k.algorithm || k.algorithm.name !== 'HKDF') {
      reply('redrop-e2e-refused')
      return
    }
    done = true
    E.keys.put(E.INTAKE_KEY, k, Date.now()).then(function () {
      reply('redrop-e2e-stored')
      window.close()
    }, function () {
      say('This browser doesn\'t let the page keep the key, so it can\'t be opened here.')
      reply('redrop-e2e-storage')
    })
  })
  reply('redrop-e2e-ready')
})()
