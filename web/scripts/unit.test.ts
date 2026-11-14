import assert from 'node:assert/strict'
import { test } from 'node:test'
import { Effect, Exit, Layer, Option, Redacted, Schema } from 'effect'
import { HttpClient, HttpClientError, HttpClientResponse } from 'effect/http'
import { AsyncResult, Atom, AtomRegistry, Reactivity } from 'effect/reactivity'
import { RedropApi, json } from '@/api/redrop-api'
import { fromProblem } from '@/api/errors'
import { safeNext } from '@/app/paths'
import * as account from '@/features/account/endpoints'
import * as auth from '@/features/auth/endpoints'
import { Artifact, ArtifactSettingsForm, filterFromSearch, settingsDefaults, toUpdateRequest } from '@/features/artifacts/schemas'
import { AdminArtifact, CreateInviteForm, toCreateInviteRequest, toUpdateUserRequest } from '@/features/admin/schemas'
import { NewAccountForm, NewPasswordForm } from '@/features/onboarding/schemas'
import { CreatedToken, TokenList } from '@/features/tokens/schemas'
import { psQuote, setupCommand, setupOsFromPlatform, shQuote } from '@/features/tokens/setup-command'

Object.defineProperty(globalThis, 'location', { configurable: true, value: { origin: 'https://app.example', pathname: '/artifacts/deep/link' } })

function harness(respond: (path: string) => Response | 'offline') {
  const requests: Array<{ path: string; method: string; csrf: string | undefined; body: string | undefined }> = []
  let rechecks = 0
  const stub = Layer.succeed(HttpClient.HttpClient, HttpClient.make((req, url) => Effect.suspend(() => {
    requests.push({ path: url.pathname + url.search, method: req.method, csrf: req.headers['x-redrop-request'],
      body: req.body._tag === 'Uint8Array' ? new TextDecoder().decode(req.body.body) : undefined })
    const response = respond(url.pathname)
    return response === 'offline'
      ? Effect.fail(new HttpClientError.HttpClientError({ reason: new HttpClientError.TransportError({ request: req, description: 'offline' }) }))
      : Effect.succeed(HttpClientResponse.fromWeb(req, response))
  })))
  const watched = Layer.effectDiscard(Reactivity.Reactivity.use((r) => Effect.sync(() => {
    r.registerUnsafe(['session'], () => rechecks++)
  })))
  const layer = RedropApi.layer.pipe(Layer.provide(stub), Layer.provideMerge(watched.pipe(Layer.provideMerge(Reactivity.layer))))
  return { run: <A, E>(effect: Effect.Effect<A, E, RedropApi>) => Effect.runPromise(effect.pipe(Effect.provide(layer))), requests, rechecks: () => rechecks }
}
const response = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } })

await test('problem status maps to the expected tagged error', () => {
  const tags = { 400: 'Invalid', 401: 'Unauthorized', 403: 'Forbidden', 404: 'NotFound', 409: 'Conflict', 410: 'Gone', 413: 'TooLarge', 429: 'RateLimited', 500: 'ServerError' }
  for (const [status, tag] of Object.entries(tags)) assert.equal(fromProblem(Number(status), 'code', 'detail')._tag, tag)
})

await test('API encodes paths, requests and queries and attaches CSRF', async () => {
  const h = harness(() => response({ value: 42 }))
  const Body = Schema.Struct({ title: Schema.String })
  const result = await h.run(RedropApi.use((api) => api.call({ method: 'PATCH', path: ['artifacts', 'a/b c'], query: { q: 'word', ignored: undefined }, body: json(Body, { title: 'title' }), response: Schema.Struct({ value: Schema.Number }) })))
  assert.equal(result.value, 42)
  assert.deepEqual(h.requests, [{ method: 'PATCH', path: '/api/v1/artifacts/a%2Fb%20c?q=word', csrf: '1', body: '{"title":"title"}' }])
})

await test('401 rechecks the session once; session probe and TOTP login do not loop', async () => {
  const h = harness(() => response({ title: 'totp_required', detail: 'Enter code' }, 401))
  await h.run(Effect.gen(function* () {
    const api = yield* RedropApi
    const err = yield* Effect.flip(api.call({ method: 'GET', path: ['artifacts'] }))
    assert.equal(err._tag, 'Unauthorized')
    assert.equal(h.rechecks(), 1)
    assert(Option.isNone(yield* api.probe({ method: 'GET', path: ['me'], response: Schema.Unknown })))
    assert.equal((yield* auth.login({ username: 'fixture', password: 'example-test-only' }))._tag, 'TotpRequired')
    assert.equal(h.rechecks(), 1)
  }))
})

await test('wrong current password rechecks me rather than assuming sign-out', async () => {
  const h = harness(() => response({ title: 'Unauthorized', detail: 'current password is wrong' }, 401))
  await h.run(Effect.flip(account.changePassword({ current_password: 'fixture', new_password: 'example-test-only' })))
  assert.equal(h.rechecks(), 1)
})

await test('proxy and network errors are typed, malformed success is a defect', async () => {
  const h = harness((p) => p.endsWith('/proxy') ? new Response('<html>upstream error</html>', { status: 502 }) : p.endsWith('/offline') ? 'offline' : response({ wrong: true }))
  await h.run(Effect.gen(function* () {
    const api = yield* RedropApi
    assert.equal((yield* Effect.flip(api.call({ method: 'GET', path: ['proxy'] })))._tag, 'ServerError')
    assert.equal((yield* Effect.flip(api.call({ method: 'GET', path: ['offline'] })))._tag, 'Offline')
    const exit = yield* Effect.exit(api.call({ method: 'GET', path: ['invalid'], response: Schema.Struct({ required: Schema.String }) }))
    assert(Exit.isFailure(exit))
  }))
})

await test('forms validate numbers and cross-field confirmation, and keep password/TTL omitted', () => {
  const input = { title: '  Report  ', ttl: 'keep', passwordAction: 'keep', password: '', maxViews: '12', resetViews: false, spa: true, infoButton: true, oldVersions: false }
  const values = Schema.decodeUnknownSync(ArtifactSettingsForm)(input)
  assert.deepEqual(toUpdateRequest(values, 'bundle'), { title: 'Report', max_views: 12, spa: true, info_button: true, viewer_old_versions: false })
  assert(!('spa' in toUpdateRequest(values, 'file')))
  for (const maxViews of ['-1', '1.5', '1000001', 'not a number']) assert(Option.isNone(Schema.decodeUnknownOption(ArtifactSettingsForm)({ ...input, maxViews })))
  assert(Option.isNone(Schema.decodeUnknownOption(NewPasswordForm)({ password: 'example-test-only', confirm: 'different' })))
  assert(Option.isNone(Schema.decodeUnknownOption(NewAccountForm)({ username: '../', password: 'example-test-only', confirm: 'example-test-only' })))
  const invite = Schema.decodeUnknownSync(CreateInviteForm)({ note: 'test', role: 'user', quotaGB: '', ttl: '7d' })
  assert(!('quota_bytes' in toCreateInviteRequest(invite)))
  assert.equal(toUpdateUserRequest({ displayName: 'name', role: 'user', quotaGB: 1.5, disabled: false }).quota_bytes, 1_500_000_000)
})

await test('viewer settings: defaults from the artifact and both are always sent explicitly', () => {
  const artifact = {
    id: 'a1', title: 'Report', kind: 'html', status: 'active', url: 'https://x.usercontent.example/', has_password: false,
    max_views: 0, views: 0, previews: 0, files: 1, size: 10, total_size: 10, version: 1, versions: 1,
    created_at: new Date(), updated_at: new Date(), expires_at: null, last_viewed_at: null, purge_at: null,
    info_button: true, viewer_old_versions: false,
  } as const
  const defaults = settingsDefaults(artifact, false)
  assert.equal(defaults.infoButton, true)
  assert.equal(defaults.oldVersions, false, 'older versions are off unless the owner enabled them')
  const unchanged = Schema.decodeUnknownSync(ArtifactSettingsForm)(defaults)
  const body = toUpdateRequest(unchanged, 'html')
  assert.equal(body.info_button, true, 'the shown value is saved explicitly')
  assert.equal(body.viewer_old_versions, false)
  const changed = Schema.decodeUnknownSync(ArtifactSettingsForm)({ ...defaults, infoButton: false, oldVersions: true })
  assert.deepEqual([toUpdateRequest(changed, 'file').info_button, toUpdateRequest(changed, 'file').viewer_old_versions], [false, true])
  const decoded = Schema.decodeUnknownSync(Artifact)({ ...artifact, created_at: '2026-11-13T12:00:00Z', updated_at: '2026-11-13T12:00:00Z' })
  assert(decoded.info_button && !decoded.viewer_old_versions)
  assert(Option.isNone(Schema.decodeUnknownOption(Artifact)({ ...artifact, viewer_old_versions: undefined, created_at: '2026-11-13T12:00:00Z', updated_at: '2026-11-13T12:00:00Z' })), 'the field is required on the wire')
})

await test('metadata decoding strips links and token lists strip secrets', () => {
  const timestamp = '2026-11-13T12:00:00Z'
  const decoded = Schema.decodeUnknownSync(AdminArtifact)({ id: 'test', title: 'Report', kind: 'html', status: 'active', owner_username: 'fixture', has_password: false, max_views: 0, views: 0, size: 1, total_size: 1, versions: 1, created_at: timestamp, expires_at: null, last_viewed_at: null, url: 'must-be-stripped' })
  assert(!('url' in decoded))
  const token = { id: 'test', name: 'fixture', prefix: 'fixture', artifacts: 0, created_at: timestamp, expires_at: null, last_used_at: null, secret: 'test-only-value' }
  assert.equal(Schema.decodeUnknownSync(CreatedToken)(token).secret.toString(), '<redacted>')
  assert.equal(Redacted.value(Schema.decodeUnknownSync(CreatedToken)(token).secret), 'test-only-value')
  assert(!('secret' in Schema.decodeUnknownSync(TokenList)({ items: [token] }).items![0]))
  assert.equal(Schema.decodeUnknownSync(TokenList)({ items: null }).items, null)
})

await test('filter fallback and safe login return targets', () => {
  assert.deepEqual(filterFromSearch(new URLSearchParams('status=bad&q=%20report%20')), { status: 'all', q: 'report' })
  for (const path of ['//evil.example', '/\\evil.example', '/\n/evil.example', 'https://evil.example', '/login']) assert.equal(safeNext(path), '/artifacts')
  assert.equal(safeNext('/artifacts?status=expired'), '/artifacts?status=expired')
})

await test('atom families deduplicate filters, invalidate reads, and Reset drops mutation results', async () => {
  let count = 0
  const rt = Atom.runtime(Layer.empty)
  const family = Atom.family((f: { q: string }) => rt.atom(Effect.sync(() => `${f.q}:${++count}`)).pipe(Atom.withReactivity(['session'])))
  assert.equal(family({ q: 'test' }), family({ q: 'test' }))
  const a = family({ q: 'test' })
  const fn = rt.fn((value: string) => Effect.succeed(value), { reactivityKeys: ['session'] })
  const registry = AtomRegistry.make()
  const unmount = registry.mount(a)
  try {
    assert.equal(await Effect.runPromise(AtomRegistry.getResult(registry, a)), 'test:1')
    registry.set(fn, 'test-only-value')
    assert.equal(await Effect.runPromise(AtomRegistry.getResult(registry, fn, { suspendOnWaiting: true })), 'test-only-value')
    assert.equal(await Effect.runPromise(AtomRegistry.getResult(registry, a, { suspendOnWaiting: true })), 'test:2')
    registry.set(fn, Atom.Reset)
    assert(AsyncResult.isInitial(registry.get(fn)))
  } finally { unmount(); registry.dispose() }
})

await test('setup command installs and logs in with the token, scoped to the installer', () => {
  assert.equal(setupCommand('unix', 'https://redrop.example.com', 'rdp_abc-123_x'),
    "curl -fsSL 'https://redrop.example.com/install.sh' | REDROP_TOKEN='rdp_abc-123_x' sh")
  assert.equal(setupCommand('windows', 'https://redrop.example.com', 'rdp_abc-123_x'),
    "$env:REDROP_TOKEN='rdp_abc-123_x'; irm 'https://redrop.example.com/install.ps1' | iex")
})

await test('setup command quotes for POSIX sh and PowerShell', () => {
  const q = "'"
  assert.equal(shQuote(''), "''")
  assert.equal(shQuote("it's"), String.raw`'it'\''s'`)
  assert.equal(shQuote(q + q), String.raw`''\'''\'''`)
  // Inside POSIX single quotes $, `, \, ", spaces and newlines are literal.
  assert.equal(shQuote('$(id) `x` \\ "a b"\n'), "'$(id) `x` \\ \"a b\"\n'")
  assert.equal(psQuote(''), "''")
  assert.equal(psQuote("it's"), "'it''s'")
  // PowerShell also ends single-quoted strings at typographic quotes U+2018..U+201B.
  assert.equal(psQuote('a‘b’c‚d‛e'), "'a‘‘b’’c‚‚d‛‛e'")
  // Inside PowerShell single quotes $ and the backtick escape are literal.
  assert.equal(psQuote('$env:X `n "y"'), `'$env:X \`n "y"'`)
  assert.equal(setupCommand('unix', 'https://redrop.example.com', "x'; rm -rf ~; '"),
    String.raw`curl -fsSL 'https://redrop.example.com/install.sh' | REDROP_TOKEN='x'\''; rm -rf ~; '\''' sh`)
  assert.equal(setupCommand('windows', 'https://redrop.example.com', "x'; Remove-Item ~; '"),
    "$env:REDROP_TOKEN='x''; Remove-Item ~; '''; irm 'https://redrop.example.com/install.ps1' | iex")
})

await test('setup OS defaults from the browser platform', () => {
  for (const p of ['Win32', 'Windows', 'win64']) assert.equal(setupOsFromPlatform(p), 'windows')
  for (const p of ['MacIntel', 'macOS', 'Linux x86_64', 'iPhone', '']) assert.equal(setupOsFromPlatform(p), 'unix')
})

await test('successful command responses are schema validated too', async () => {
  const h = harness(() => response({ wrong: true }))
  const exit = await h.run(Effect.exit(RedropApi.use((api) => api.call({ method: 'POST', path: ['auth', 'logout'] }))))
  assert(Exit.isFailure(exit))
})
