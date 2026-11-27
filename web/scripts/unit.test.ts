import assert from 'node:assert/strict'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { createJiti } from 'jiti'
import { createElement, type ReactNode } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { RegistryContext } from '@effect/atom-react'
import { Effect, Exit, Layer, Option, Redacted, Schema } from 'effect'
import { HttpClient, HttpClientError, HttpClientResponse } from 'effect/http'
import { AsyncResult, Atom, AtomRegistry, Reactivity } from 'effect/reactivity'
import { RedropApi, json } from '@/api/redrop-api'
import { fromProblem } from '@/api/errors'
import { safeNext } from '@/app/paths'
import * as account from '@/features/account/endpoints'
import * as auth from '@/features/auth/endpoints'
import * as authAtoms from '@/features/auth/atoms'
import { AuthConfig } from '@/features/auth/schemas'
import * as adminAtoms from '@/features/admin/atoms'
import type {} from '@/features/artifacts/atoms'
import * as artifactEndpoints from '@/features/artifacts/endpoints'
import { Artifact, ArtifactSettingsForm, Comment, CommentList, filterFromSearch, groupByVersion, settingsDefaults, shareLink, statusLabel, toUpdateRequest, tunnelState } from '@/features/artifacts/schemas'
import { Stats, AdminArtifact, CreateInviteForm, createInviteDefaults, Invite, Report, ReportList, reportFilterFromSearch, reportSearch, reportedLink, toCreateInviteRequest, toUpdateUserRequest } from '@/features/admin/schemas'
import * as adminEndpoints from '@/features/admin/endpoints'
import * as reportEndpoints from '@/features/reports/endpoints'
import { ReportForm, reportDefaults, toReportRequest } from '@/features/reports/schemas'
import { NewAccountForm, NewPasswordForm } from '@/features/onboarding/schemas'
import { CreatedToken, TokenList } from '@/features/tokens/schemas'
import { psQuote, setupCommand, setupOsFromPlatform, shQuote } from '@/features/tokens/setup-command'

Object.defineProperty(globalThis, 'location', { configurable: true, value: { origin: 'https://app.example', pathname: '/artifacts/deep/link' } })

// The shared script runner handles TypeScript; component checks also need JSX.
const jsx = createJiti(import.meta.url, {
  alias: { '@': fileURLToPath(new URL('../src', import.meta.url)) },
  jsx: { runtime: 'automatic' },
})
const { InviteForm } = await jsx.import<typeof import('@/features/admin/components/create-invite-dialog')>('../src/features/admin/components/create-invite-dialog.tsx')
const { Component: InvitesPage } = await jsx.import<typeof import('@/features/admin/pages/invites-page')>('../src/features/admin/pages/invites-page.tsx')

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

await test('admin statistics retain received staging and reserved bytes separately', () => {
  const value = { users: 1, artifacts: 2, active_artifacts: 1, logical_bytes: 100, stored_bytes: 80, staged_bytes: 20, reserved_bytes: 50, max_total_bytes: 1000, max_version_bytes: 500 }
  assert.deepEqual(Schema.decodeUnknownSync(Stats)(value), value)
  assert(Option.isNone(Schema.decodeUnknownOption(Stats)({ ...value, staged_bytes: '20' })))
  assert(Option.isNone(Schema.decodeUnknownOption(Stats)({ ...value, reserved_bytes: undefined })))
})

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
  const input = { title: '  Report  ', ttl: 'keep', passwordAction: 'keep', password: '', maxViews: '12', resetViews: false, spa: true, infoButton: true, oldVersions: false, feedback: false }
  const values = Schema.decodeUnknownSync(ArtifactSettingsForm)(input)
  assert.deepEqual(toUpdateRequest(values, 'bundle'), { title: 'Report', max_views: 12, spa: true, info_button: true, viewer_old_versions: false, feedback: false })
  assert(!('spa' in toUpdateRequest(values, 'file')))
  for (const maxViews of ['-1', '1.5', '1000001', 'not a number']) assert(Option.isNone(Schema.decodeUnknownOption(ArtifactSettingsForm)({ ...input, maxViews })))
  assert(Option.isNone(Schema.decodeUnknownOption(NewPasswordForm)({ password: 'example-test-only', confirm: 'different' })))
  assert(Option.isNone(Schema.decodeUnknownOption(NewAccountForm)({ username: '../', password: 'example-test-only', confirm: 'example-test-only' })))
  const invite = Schema.decodeUnknownSync(CreateInviteForm)({ ...createInviteDefaults, note: 'test' })
  assert(!('quota_bytes' in toCreateInviteRequest(invite)))
  assert.equal(toUpdateUserRequest({ displayName: 'name', role: 'user', quotaGB: 1.5, disabled: false }).quota_bytes, 1_500_000_000)
})

await test('invite email form accepts empty or one bare address and omits empty email', () => {
  for (const email of ['', '   ']) {
    const values = Schema.decodeUnknownSync(CreateInviteForm)({ ...createInviteDefaults, email })
    assert.deepEqual(toCreateInviteRequest(values), { note: '', role: 'user', ttl: '7d' })
  }
  for (const email of ['recipient@example.com', 'first.last+invites@sub.example.com', 'recipient@localhost']) {
    const values = Schema.decodeUnknownSync(CreateInviteForm)({ ...createInviteDefaults, note: ' Welcome ', email: ` ${email} ` })
    assert.deepEqual(toCreateInviteRequest(values), { note: 'Welcome', role: 'user', ttl: '7d', email })
  }
  for (const email of ['not-an-email', '@example.com', 'recipient@', 'a@@example.com', 'a b@example.com', 'Name <a@example.com>', 'a@example.com,b@example.com', 'a@example.com\r\nBcc: b@example.com', '.a@example.com', 'a..b@example.com', 'a@example..com', 'a@-example.com', 'a'.repeat(243) + '@example.com']) {
    assert(Option.isNone(Schema.decodeUnknownOption(CreateInviteForm)({ ...createInviteDefaults, email })), email)
  }
  assert(Option.isSome(Schema.decodeUnknownOption(CreateInviteForm)({ ...createInviteDefaults, email: 'a'.repeat(242) + '@example.com' })))
})

const inviteWire = (over: Record<string, unknown> = {}) => ({
  id: 'i1', note: 'Welcome', role: 'user', quota_bytes: 0,
  created_at: '2026-11-20T10:00:00Z', expires_at: '2026-12-20T10:00:00Z', used_at: null, ...over,
})
const authConfigWire = { version: 'fixture', setup_required: false, password_login: true, providers: null, admin_requires_sso: false, mail: true, public_url: 'https://app.example', content_url: 'https://usercontent.example' }

await test('mail capability is a required boolean and invite delivery metadata is optional', () => {
  for (const mail of [true, false]) assert.equal(Schema.decodeUnknownSync(AuthConfig)({ ...authConfigWire, mail }).mail, mail)
  for (const mail of [undefined, 'true', null]) assert(Option.isNone(Schema.decodeUnknownOption(AuthConfig)({ ...authConfigWire, mail })))
  const plain = Schema.decodeUnknownSync(Invite)(inviteWire())
  assert.equal(plain.email, undefined)
  assert.equal(plain.email_status, undefined)
  for (const email_status of ['queued', 'sending', 'sent', 'failed']) {
    const invite = Schema.decodeUnknownSync(Invite)(inviteWire({ email: 'recipient@example.com', email_status, email_error: 'Delivery rejected', email_at: '2026-11-20T10:01:00Z' }))
    assert.equal(invite.email_status, email_status)
    assert.equal(invite.email_error, 'Delivery rejected')
    assert.equal(invite.email_at?.toISOString(), '2026-11-20T10:01:00.000Z')
  }
  assert.equal(Schema.decodeUnknownSync(Invite)(inviteWire({ email_at: null })).email_at, null)
  for (const over of [{ email_status: 'unknown' }, { email_at: 'not a date' }, { email_error: 42 }, { email: 42 }]) {
    assert(Option.isNone(Schema.decodeUnknownOption(Invite)(inviteWire(over))))
  }
})

await test('invite endpoints encode email on creation and decode the resend Invite response', async () => {
  const wire = inviteWire({ email: 'recipient@example.com', email_status: 'queued' })
  const h = harness(() => response({ ...wire, url: 'https://app.example/invite/test-only' }))
  const req = toCreateInviteRequest(Schema.decodeUnknownSync(CreateInviteForm)({ ...createInviteDefaults, email: 'recipient@example.com' }))
  const created = await h.run(adminEndpoints.createInvite(req))
  assert.equal(Redacted.value(created.url), 'https://app.example/invite/test-only')
  const resent = await h.run(adminEndpoints.resendInvite('i/1'))
  assert.equal(resent.email_status, 'queued')
  assert(resent.expires_at instanceof Date)
  assert(!('url' in resent), 'resending does not expose the one-time secret')
  assert.deepEqual(h.requests, [
    { method: 'POST', path: '/api/v1/admin/invites', csrf: '1', body: JSON.stringify(req) },
    { method: 'POST', path: '/api/v1/admin/invites/i%2F1/resend', csrf: '1', body: undefined },
  ])
})

function renderInvites(node: ReactNode, mail: boolean, invites: ReadonlyArray<Invite> = [], waiting = false) {
  const registry = AtomRegistry.make({ initialValues: [
    [authAtoms.config, AsyncResult.success(Schema.decodeUnknownSync(AuthConfig)({ ...authConfigWire, mail }))],
    [adminAtoms.invites, AsyncResult.success(invites, { waiting })],
  ] })
  try { return renderToStaticMarkup(createElement(RegistryContext.Provider, { value: registry }, node)) }
  finally { registry.dispose() }
}

await test('invite form shows email only when mail is enabled and explains who sees the note', () => {
  const form = createElement(InviteForm, { onCreated: () => {} })
  const enabled = renderInvites(form, true)
  assert.match(enabled, /name="email"/)
  assert.match(enabled, /type="email"/)
  assert.match(enabled, /Email \(optional\)/)
  assert.match(enabled, /Included in the email if you enter a recipient/)
  const disabled = renderInvites(form, false)
  assert.doesNotMatch(disabled, /name="email"/)
  assert.match(disabled, /Only admins see it/)
})

await test('invite list renders delivery states and failure reasons as text', (t) => {
  t.mock.method(Date, 'now', () => Date.parse('2026-11-27T12:00:00Z'))
  for (const email_status of ['queued', 'sending', 'sent', 'failed']) {
    const invite = Schema.decodeUnknownSync(Invite)(inviteWire({ email: 'recipient@example.com', email_status, email_error: '<img src=x onerror=alert(1)>', email_at: '2026-11-20T10:01:00Z' }))
    const html = renderInvites(createElement(InvitesPage), true, [invite])
    assert.match(html, /Email status/)
    assert.match(html, /recipient@example.com/)
    assert(html.includes(`>${email_status}</span>`))
    if (email_status === 'failed') assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/)
    else assert.doesNotMatch(html, /onerror/)
    assert.doesNotMatch(html, /<img/)
  }
})

await test('Send again is limited to unused, unexpired emailed invites while mail is enabled', (t) => {
  t.mock.method(Date, 'now', () => Date.parse('2026-11-27T12:00:00Z'))
  const active = Schema.decodeUnknownSync(Invite)(inviteWire({ email: 'recipient@example.com', email_status: 'sent' }))
  const page = createElement(InvitesPage)
  assert.match(renderInvites(page, true, [active]), />Send again</)
  assert.doesNotMatch(renderInvites(page, false, [active]), />Send again</)
  for (const over of [{ email: undefined }, { email: '' }, { used_at: new Date('2026-11-26T12:00:00Z') }, { expires_at: new Date('2026-11-26T12:00:00Z') }]) {
    assert.doesNotMatch(renderInvites(page, true, [{ ...active, ...over }]), />Send again</)
  }
  assert.match(renderInvites(page, true, [active], true), /disabled=""[^>]*>Send again</)
  assert.match(renderInvites(page, true, [Schema.decodeUnknownSync(Invite)(inviteWire())]), /No email/)
})

await test('viewer settings: defaults from the artifact and both are always sent explicitly', () => {
  const artifact = {
    id: 'a1', title: 'Report', kind: 'html', status: 'active', url: 'https://x.usercontent.example/', has_password: false,
    max_views: 0, views: 0, previews: 0, files: 1, size: 10, total_size: 10, version: 1, versions: 1,
    created_at: new Date(), updated_at: new Date(), expires_at: null, last_viewed_at: null, purge_at: null,
    info_button: true, viewer_old_versions: false, feedback: false, revoked_by_admin: false, e2e: false, online: false,
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

await test('share links use the key intake for e2e and the content URL for tunnels and other artifacts', () => {
  const link = { url: 'https://x.usercontent.example/', e2e: false, e2e_link: 'https://app.example/e/a1' }
  assert.equal(shareLink(link), link.url)
  assert.equal(shareLink({ ...link, e2e: true }), link.e2e_link)
  assert.equal(shareLink({ url: link.url, e2e: true }), link.url)
})

await test('tunnels: decode as their own kind and never send a view limit or older versions', () => {
  const tunnel = Schema.decodeUnknownSync(Artifact)({
    id: 't1', title: 'localhost:3000', kind: 'tunnel', status: 'active', url: 'https://x.usercontent.example/', has_password: true,
    max_views: 0, views: 0, previews: 0, files: 0, size: 0, total_size: 0, version: 1, versions: 1,
    created_at: '2026-11-13T12:00:00Z', updated_at: '2026-11-13T12:00:00Z', expires_at: null, last_viewed_at: null, purge_at: null,
    info_button: true, viewer_old_versions: false, feedback: false, revoked_by_admin: false, e2e: false, online: true,
  })
  assert.equal(tunnelState(tunnel), 'online')
  assert.equal(tunnelState({ ...tunnel, online: false }), 'offline')
  const values = Schema.decodeUnknownSync(ArtifactSettingsForm)({ ...settingsDefaults(tunnel, false), resetViews: true, ttl: '1d' })
  const body = toUpdateRequest(values, 'tunnel')
  assert.deepEqual(body, { title: 'localhost:3000', ttl: '1d', info_button: true, feedback: false })
  const wire: Record<string, unknown> = { ...Schema.encodeSync(Artifact)(tunnel) }
  delete wire.online
  assert(Option.isNone(Schema.decodeUnknownOption(Artifact)(wire)), 'online is required on the wire')
})

await test('feedback setting: defaults from the artifact, always sent explicitly, required on the wire', () => {
  const artifact = Schema.decodeUnknownSync(Artifact)({
    id: 'a1', title: 'Report', kind: 'html', status: 'active', url: 'https://x.usercontent.example/', has_password: false,
    max_views: 0, views: 0, previews: 0, files: 1, size: 10, total_size: 10, version: 1, versions: 1,
    created_at: '2026-11-13T12:00:00Z', updated_at: '2026-11-13T12:00:00Z', expires_at: null, last_viewed_at: null, purge_at: null,
    info_button: true, viewer_old_versions: false, feedback: true, revoked_by_admin: false, e2e: false, online: false,
  })
  const defaults = settingsDefaults(artifact, false)
  assert.equal(defaults.feedback, true)
  assert.equal(toUpdateRequest(Schema.decodeUnknownSync(ArtifactSettingsForm)(defaults), 'html').feedback, true, 'the shown value is saved explicitly')
  assert.equal(toUpdateRequest(Schema.decodeUnknownSync(ArtifactSettingsForm)({ ...defaults, feedback: false }), 'file').feedback, false)
  const wire: Record<string, unknown> = { ...Schema.encodeSync(Artifact)(artifact) }
  delete wire.feedback
  assert(Option.isNone(Schema.decodeUnknownOption(Artifact)(wire)), 'the field is required on the wire')
})

const commentWire = (over: Record<string, unknown> = {}) => ({
  id: 'c1', version: 1, page: '/report.html#totals', body: '<img src=x onerror=alert(1)>\nline two', name: '',
  quote: null, created_at: '2026-11-13T12:00:00Z', resolved_at: null, ...over,
})

await test('comments decode with or without a quote and resolution, keeping text as is', () => {
  const open = Schema.decodeUnknownSync(Comment)(commentWire())
  assert.equal(open.quote, null)
  assert.equal(open.resolved_at, null)
  assert.equal(open.body, '<img src=x onerror=alert(1)>\nline two', 'text is data, not markup')
  const done = Schema.decodeUnknownSync(Comment)(commentWire({ quote: { exact: 'totals', prefix: 'The ', suffix: ' are' }, resolved_at: '2026-11-14T08:00:00Z' }))
  assert.deepEqual(done.quote, { exact: 'totals', prefix: 'The ', suffix: ' are' })
  assert(done.resolved_at instanceof Date)
  assert(Option.isNone(Schema.decodeUnknownOption(Comment)(commentWire({ quote: { exact: 'x' } }))), 'a quote has all three fields')
  assert.equal(Schema.decodeUnknownSync(CommentList)({ items: null }).items, null)
})

await test('comments group by version, newest version first, oldest comment first', () => {
  const c = (id: string, version: number, at: string) => Schema.decodeUnknownSync(Comment)(commentWire({ id, version, created_at: at }))
  const groups = groupByVersion([c('a', 1, '2026-11-13T12:00:00Z'), c('b', 3, '2026-11-13T14:00:00Z'), c('c', 1, '2026-11-13T10:00:00Z'), c('d', 3, '2026-11-13T13:00:00Z')])
  assert.deepEqual(groups.map((g) => [g.version, g.comments.map((x) => x.id)]), [[3, ['d', 'b']], [1, ['c', 'a']]])
  assert.deepEqual(groupByVersion([]), [])
})

await test('feedback endpoints: list by status, resolve with a body, delete', async () => {
  const h = harness((path) => path.endsWith('/feedback') ? response({ items: [commentWire()] }) : path.endsWith('/c%2F1') && h.requests.at(-1)?.method === 'PATCH' ? response(commentWire({ resolved_at: '2026-11-14T08:00:00Z' })) : response({ ok: true }))
  const list = await h.run(artifactEndpoints.feedback('a 1', 'open'))
  assert.equal(list.length, 1)
  const resolved = await h.run(artifactEndpoints.resolveComment('a1', 'c/1', true))
  assert(resolved.resolved_at instanceof Date)
  await h.run(artifactEndpoints.deleteComment('a1', 'c/1'))
  assert.deepEqual(h.requests.map(({ method, path, body }) => ({ method, path, body })), [
    { method: 'GET', path: '/api/v1/artifacts/a%201/feedback?status=open', body: undefined },
    { method: 'PATCH', path: '/api/v1/artifacts/a1/feedback/c%2F1', body: '{"resolved":true}' },
    { method: 'DELETE', path: '/api/v1/artifacts/a1/feedback/c%2F1', body: undefined },
  ])
  const empty = harness(() => response({ items: null }))
  assert.deepEqual(await empty.run(artifactEndpoints.feedback('a1', 'all')), [])
})

await test('an administrator\'s revocation is named and required on the wire', () => {
  assert.equal(statusLabel({ status: 'revoked', revoked_by_admin: true }), 'revoked by an administrator')
  assert.equal(statusLabel({ status: 'revoked', revoked_by_admin: false }), 'revoked')
  assert.equal(statusLabel({ status: 'active', revoked_by_admin: false }), 'active')
  const timestamp = '2026-11-13T12:00:00Z'
  const meta = { id: 'test', title: 'Report', kind: 'html', status: 'revoked', owner_username: 'fixture', has_password: false, max_views: 0, views: 0, size: 1, total_size: 1, versions: 1, created_at: timestamp, expires_at: null, last_viewed_at: null, e2e: false, online: false }
  assert(Schema.decodeUnknownSync(AdminArtifact)({ ...meta, revoked_by_admin: true }).revoked_by_admin)
  assert(Option.isNone(Schema.decodeUnknownOption(AdminArtifact)(meta)), 'the field is required on the wire')
})

await test('metadata decoding strips links and token lists strip secrets', () => {
  const timestamp = '2026-11-13T12:00:00Z'
  const decoded = Schema.decodeUnknownSync(AdminArtifact)({ id: 'test', title: 'Report', kind: 'html', status: 'active', owner_username: 'fixture', has_password: false, max_views: 0, views: 0, size: 1, total_size: 1, versions: 1, created_at: timestamp, expires_at: null, last_viewed_at: null, revoked_by_admin: false, e2e: true, online: false, url: 'must-be-stripped' })
  assert(!('url' in decoded))
  assert(decoded.e2e, 'admins see that an artifact is end-to-end encrypted')
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

await test('report form: the page comes from the fragment, limits count characters, empty fields are left out', () => {
  const page = 'https://amber-otter-quilt-river.usercontent.example/a.html?x=1&y=2'
  assert.equal(reportDefaults('#url=' + encodeURIComponent(page)).url, page)
  assert.equal(reportDefaults('').url, '')
  assert.equal(reportDefaults('#other=1').url, '')
  const decode = Schema.decodeUnknownOption(ReportForm)
  const ok = { url: '  ' + page + ' ', category: 'spam', details: ' ', contact: '' }
  const values = Option.getOrThrow(decode(ok))
  assert.deepEqual(toReportRequest(values), { url: page, category: 'spam' })
  assert.deepEqual(toReportRequest({ ...values, details: 'd', contact: 'c' }), { url: page, category: 'spam', details: 'd', contact: 'c' })
  // 2000 astral characters are 4000 UTF-16 units but within the limit.
  assert(Option.isSome(decode({ ...ok, details: '😀'.repeat(2000) })))
  assert(Option.isNone(decode({ ...ok, details: 'a'.repeat(2001) })))
  assert(Option.isNone(decode({ ...ok, contact: 'a'.repeat(201) })))
  assert(Option.isNone(decode({ ...ok, url: '   ' })))
  assert(Option.isNone(decode({ ...ok, category: 'rude' })))
})

const reportWire = (over: Record<string, unknown> = {}) => ({
  id: 'r1', source: 'visitor', category: 'phishing', details: '<img src=x onerror=alert(1)>', contact: 'sam', reported_url: 'https://amber-otter-quilt-river.usercontent.example/',
  version: 0, status: 'open', created_at: '2026-11-18T10:00:00Z', closed_at: null, closed_by: '', artifact_id: 'a1', artifact: null,
  owner_username: 'bob', owner_suspended: false, history: [{ at: '2026-11-18T10:05:00Z', actor: 'admin', action: 'report.dismissed', detail: '' }], ...over,
})

await test('admin reports decode; the artifact stays metadata only; only http(s) links are links', () => {
  const artifact = { id: 'a1', title: 't', kind: 'html', status: 'revoked', owner_username: 'bob', has_password: false, max_views: 0, views: 0, size: 1, total_size: 1,
    versions: 1, e2e: false, created_at: '2026-11-18T10:00:00Z', expires_at: null, last_viewed_at: null, revoked_by_admin: true, online: false, url: 'https://secret.example/' }
  const r = Schema.decodeUnknownSync(Report)(reportWire({ artifact }))
  assert.equal(r.details, '<img src=x onerror=alert(1)>')
  assert(r.artifact && !('url' in r.artifact))
  assert.equal(r.history?.[0].at.toISOString(), '2026-11-18T10:05:00.000Z')
  assert.equal(Schema.decodeUnknownSync(Report)(reportWire({ history: null })).history, null)
  assert(Option.isNone(Schema.decodeUnknownOption(Report)(reportWire({ status: 'pending' }))))
  assert.equal(reportedLink(r), 'https://amber-otter-quilt-river.usercontent.example/')
  for (const bad of ['javascript:alert(1)', 'data:text/html,x', '//evil.example/', '']) assert.equal(reportedLink({ reported_url: bad }), undefined)
})

await test('report queue filters validate URL state and reset paging on a filter change', () => {
  assert.deepEqual(reportFilterFromSearch(new URLSearchParams('status=dismissed&source=system&page=12')), { status: 'dismissed', source: 'system', page: 12 })
  assert.deepEqual(reportFilterFromSearch(new URLSearchParams('status=bogus&source=bogus&page=0')), { status: 'open', source: 'all', page: 1 })
  assert.deepEqual(reportFilterFromSearch(new URLSearchParams('')), { status: 'open', source: 'all', page: 1 })
  for (const page of ['-1', '1.5', 'Infinity', 'NaN', '2x', '9007199254740992']) {
    assert.equal(reportFilterFromSearch(new URLSearchParams({ page })).page, 1)
  }
  const current = new URLSearchParams('status=resolved&source=visitor&page=8&keep=yes')
  assert.equal(reportSearch(current, { status: 'dismissed' }).toString(), 'status=dismissed&source=visitor&keep=yes')
  assert.equal(reportSearch(current, { source: 'system' }).toString(), 'status=resolved&source=system&keep=yes')
  assert.equal(reportSearch(current, { page: 9 }).toString(), 'status=resolved&source=visitor&page=9&keep=yes')
  assert.equal(reportSearch(current, { status: 'open', source: 'all' }).toString(), 'keep=yes')
  assert.equal(current.get('page'), '8')
})

await test('report queue pages require a total and preserve the paging metadata', () => {
  const page = Schema.decodeUnknownSync(ReportList)({ items: [reportWire()], total: 501, page: 11, page_size: 50 })
  assert.equal(page.items?.length, 1)
  assert.equal(page.total, 501)
  assert.equal(page.page, 11)
  assert.equal(page.page_size, 50)
  for (const invalid of [
    { items: [] },
    { items: [], total: -1, page: 1, page_size: 50 },
    { items: [], total: 0, page: 0, page_size: 50 },
    { items: [], total: 0, page: 1, page_size: 0 },
    { items: [], total: 0.5, page: 1, page_size: 50 },
  ]) assert(Option.isNone(Schema.decodeUnknownOption(ReportList)(invalid)))
})

await test('report endpoints: page and source queries, bodies and the CSRF header', async () => {
  const h = harness((path) => (path.startsWith('/api/v1/admin/reports') && !path.endsWith('/reports') ? response(reportWire()) : path.endsWith('/admin/reports') ? response({ items: null, total: 501, page: 11, page_size: 50 }) : response({ ok: true }, 202)))
  await h.run(reportEndpoints.submit({ url: 'https://x.example/', category: 'spam' }))
  assert.deepEqual(await h.run(adminEndpoints.reports('all', 'visitor', 11)), { items: [], total: 501, page: 11, page_size: 50 })
  await h.run(adminEndpoints.setReportStatus('r/1', { status: 'resolved' }))
  await h.run(adminEndpoints.revokeReported('r1'))
  await h.run(adminEndpoints.suspendReportedOwner('r1'))
  assert.deepEqual(h.requests, [
    { method: 'POST', path: '/api/v1/reports', csrf: '1', body: '{"url":"https://x.example/","category":"spam"}' },
    { method: 'GET', path: '/api/v1/admin/reports?status=all&source=visitor&page=11', csrf: '1', body: undefined },
    { method: 'PATCH', path: '/api/v1/admin/reports/r%2F1', csrf: '1', body: '{"status":"resolved"}' },
    { method: 'POST', path: '/api/v1/admin/reports/r1/revoke', csrf: '1', body: undefined },
    { method: 'POST', path: '/api/v1/admin/reports/r1/suspend', csrf: '1', body: undefined },
  ])
})
