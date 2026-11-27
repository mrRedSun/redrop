// redrop's key intake, on the app origin (DESIGN.md "End-to-end encrypted
// links", "Where the key goes in the browser"). Served as /e2e/intake.js
// after the decryptor (e2e.js). An end-to-end encrypted link is
// https://<app>/e/<slug>#k=<key>[&<rest>]: no artifact runs on this origin,
// and no window an artifact holds can read this page's address. The script
// takes the key off the address bar at once, imports it as a non-extractable
// CryptoKey and, when the visitor presses Open, opens a small helper window
// on the artifact's origin and hands it that CryptoKey by postMessage. Only
// the CryptoKey crosses origins, never the key's text. Once the helper has
// stored it, this tab goes to the artifact, which finds the key there.
;(function () {
  'use strict'
  var E = window.RedropE2E
  var page = document.getElementById('rd-intake')
  var msg = document.getElementById('rd-intake-msg')
  var button = document.getElementById('rd-intake-open')
  var note = document.getElementById('rd-intake-note')
  var content = page.getAttribute('data-content')
  var contentOrigin = new URL(content).origin
  var hash = location.hash.slice(1)
  // The key leaves the address bar first, before anything asynchronous.
  history.replaceState(null, '', location.pathname)
  function say(text) { msg.textContent = text }
  var DAMAGED = 'This link\'s key is damaged. Open the complete link you were given, including the part after #.'
  var parts = E ? E.splitFragment(hash) : { key: null, rest: '', damaged: true }
  if (parts.damaged) {
    say(DAMAGED)
    return
  }
  if (!parts.key) {
    say('This link\'s key is missing. Open the complete link you were given, including the part after #.')
    return
  }
  if (!window.isSecureContext || !window.crypto || !crypto.subtle) {
    say('This page is end-to-end encrypted. Browsers decrypt only on secure (HTTPS) pages, so it can\'t be opened here.')
    return
  }
  var rest = parts.rest, key = null, popup = null, timer = 0
  function retry(text) {
    clearTimeout(timer)
    popup = null
    button.disabled = false
    say(text)
  }
  window.addEventListener('message', function (e) {
    // Only the helper this page opened, on the artifact's origin.
    if (!popup || e.source !== popup || e.origin !== contentOrigin || !e.data) return
    var type = e.data.type
    if (type === 'redrop-e2e-ready') {
      popup.postMessage({ type: 'redrop-e2e-key', key: key }, contentOrigin)
    } else if (type === 'redrop-e2e-stored') {
      clearTimeout(timer)
      say('Opening the page…')
      location.replace(content + (rest ? '#' + rest : ''))
    } else if (type === 'redrop-e2e-storage') {
      retry('This browser doesn\'t let the page keep the key (some private windows block site storage), so it can\'t be opened here.')
    } else if (type === 'redrop-e2e-refused') {
      retry('The page could not take the key. Press Open to try again.')
    }
  })
  button.addEventListener('click', function () {
    if (!key || popup) return
    popup = window.open(content + '__redrop/e2e-intake', '_blank', 'popup,width=480,height=320')
    if (!popup) {
      retry('Your browser blocked the small window. Allow pop-ups for this site, then press Open again.')
      return
    }
    button.disabled = true
    say('Keeping the key for the page…')
    timer = setTimeout(function () { retry('The page could not be opened. Press Open to try again.') }, 30000)
  })
  E.importKey(parts.key).then(function (k) {
    key = k
    button.hidden = false
    note.hidden = false
  }, function () { say(DAMAGED) })
})()
