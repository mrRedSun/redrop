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
import { ArtifactSettingsForm, filterFromSearch, toUpdateRequest } from '@/features/artifacts/schemas'
import { AdminArtifact, CreateInviteForm, toCreateInviteRequest, toUpdateUserRequest } from '@/features/admin/schemas'
import { NewAccountForm, NewPasswordForm } from '@/features/onboarding/schemas'
import { CreatedToken, TokenList, cliSetupCommands } from '@/features/tokens/schemas'

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
  const input = { title: '  Report  ', ttl: 'keep', passwordAction: 'keep', password: '', maxViews: '12', resetViews: false, spa: true }
  const values = Schema.decodeUnknownSync(ArtifactSettingsForm)(input)
  assert.deepEqual(toUpdateRequest(values, 'bundle'), { title: 'Report', max_views: 12, spa: true })
  assert(!('spa' in toUpdateRequest(values, 'file')))
  for (const maxViews of ['-1', '1.5', '1000001', 'not a number']) assert(Option.isNone(Schema.decodeUnknownOption(ArtifactSettingsForm)({ ...input, maxViews })))
  assert(Option.isNone(Schema.decodeUnknownOption(NewPasswordForm)({ password: 'example-test-only', confirm: 'different' })))
  assert(Option.isNone(Schema.decodeUnknownOption(NewAccountForm)({ username: '../', password: 'example-test-only', confirm: 'example-test-only' })))
  const invite = Schema.decodeUnknownSync(CreateInviteForm)({ note: 'test', role: 'user', quotaGB: '', ttl: '7d' })
  assert(!('quota_bytes' in toCreateInviteRequest(invite)))
  assert.equal(toUpdateUserRequest({ displayName: 'name', role: 'user', quotaGB: 1.5, disabled: false }).quota_bytes, 1_500_000_000)
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

await test('CLI setup commands prompt for the token instead of putting it into history', () => {
  const commands = cliSetupCommands('https://app.example')
  assert.equal(commands.login, "redrop login --url 'https://app.example'")
  assert(!commands.login.includes('--token'))
  assert(commands.install.includes("'https://app.example/install.sh'"))
})

await test('successful command responses are schema validated too', async () => {
  const h = harness(() => response({ wrong: true }))
  const exit = await h.run(Effect.exit(RedropApi.use((api) => api.call({ method: 'POST', path: ['auth', 'logout'] }))))
  assert(Exit.isFailure(exit))
})
