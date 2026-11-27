import assert from 'node:assert/strict'
import { test } from 'node:test'
import { Schema } from 'effect'
import { ReportForm, ReportRequest, reportDefaults, toReportRequest } from '../src/features/reports/schemas'

const slug = 'amber-otter-quilt-river'
const fragment = '#k=report-test-key'

await test('report requests strip fragments from every URL input, including malformed URLs', () => {
  const inputs = [
    `https://app.example/e/${slug}`,
    `https://app.example/e/${slug}/`,
    `https://${slug}.usercontent.example/page.html?x=1`,
    'https://evil.example/anything',
    'not a URL',
    'https://app.example/e/%zz',
    '',
  ]
  for (const url of inputs) {
    const request = toReportRequest({ url: url + fragment, category: 'spam', details: '', contact: '' })
    // Boolean assertions keep a fragment out of failure output too.
    assert(request.url === url, 'report request did not strip the entire URL fragment')
    const body = JSON.stringify(Schema.encodeSync(ReportRequest)(request))
    assert(!body.includes('report-test-key') && !body.includes('#'), 'report request body contains a fragment')
  }
})

await test('prefilled and pasted intake links produce the same keyless request without changing other fields', () => {
  const url = `https://app.example/e/${slug}`
  const prefilled = reportDefaults('#url=' + encodeURIComponent(url + fragment))
  const decode = Schema.decodeUnknownSync(ReportForm)
  for (const input of [prefilled, { ...prefilled, url: `  ${url}${fragment}  ` }]) {
    const values = decode({ ...input, category: 'other', details: 'A harmful page', contact: 'reporter@example.org' })
    const request = toReportRequest(values)
    assert(request.url === url, 'prefilled or pasted report retained its fragment')
    assert.deepEqual({ ...request, url: '' }, { url: '', category: 'other', details: 'A harmful page', contact: 'reporter@example.org' })
  }
})

await test('report requests preserve query strings, encoded hashes, and already keyless URLs', () => {
  const url = `https://${slug}.usercontent.example/page%23name.html?x=%23value`
  for (const input of [url, url + '#', url + '#section#k=report-test-key']) {
    const request = toReportRequest({ url: input, category: 'phishing', details: '', contact: '' })
    assert(request.url === url, 'report URL changed outside its fragment')
    assert.deepEqual(Object.keys(request).sort(), ['category', 'url'])
  }
})
