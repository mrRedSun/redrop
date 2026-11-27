/**
 * Contract check: runs the dashboard's real endpoint modules (RedropApi +
 * hand-written Schemas) against a running server and fails if any response
 * does not decode. Guards against drift between the Go API and the
 * frontend Schemas, which no generator checks.
 *
 *   BASE=http://host:8080 E2E_USER=admin E2E_PASSWORD=... npm run check:contract
 *
 * Creates and deletes one API token and one invite; otherwise read-only.
 */
import { Cause, Effect, Exit, Layer, Option, Redacted } from 'effect'
import { FetchHttpClient } from 'effect/http'
import { Reactivity } from 'effect/reactivity'
import { RedropApi } from '@/api/redrop-api'
import * as account from '@/features/account/endpoints'
import * as admin from '@/features/admin/endpoints'
import * as artifacts from '@/features/artifacts/endpoints'
import { settingsDefaults, ArtifactSettingsForm, toUpdateRequest } from '@/features/artifacts/schemas'
import * as auth from '@/features/auth/endpoints'
import * as reports from '@/features/reports/endpoints'
import * as tokens from '@/features/tokens/endpoints'
import { Schema } from 'effect'

const base = process.env.BASE ?? 'http://127.0.0.1:8080'
const user = process.env.E2E_USER ?? 'admin'
const password = process.env.E2E_PASSWORD ?? 'correct-horse-battery'

// HttpClient resolves relative URLs against `location` in the browser.
;(globalThis as { location?: unknown }).location = { origin: base, pathname: '/', href: base + '/' }

// fetch with a cookie jar and the browser's same-origin header, like the dashboard.
// A browser also sends Origin with every POST; the report endpoint requires
// it to be the server's public origin (read from /auth/config below).
let cookie = ''
let publicOrigin = ''
const browserFetch: typeof fetch = async (input, init) => {
  const headers = new Headers(init?.headers)
  if (cookie) headers.set('cookie', cookie)
  headers.set('sec-fetch-site', 'same-origin')
  if (publicOrigin && init?.method === 'POST' && String(input instanceof Request ? input.url : input).endsWith('/api/v1/reports')) headers.set('origin', publicOrigin)
  const res = await fetch(input, { ...init, headers })
  const set = res.headers.getSetCookie().map((c) => c.split(';')[0])
  if (set.length) cookie = set.join('; ')
  return res
}

const Live = RedropApi.layer.pipe(
  Layer.provide(FetchHttpClient.layer),
  Layer.provide(Layer.succeed(FetchHttpClient.Fetch, browserFetch)),
  Layer.provideMerge(Reactivity.layer),
)

const checks: Array<[string, boolean]> = []
const check = (name: string, ok: boolean) => checks.push([name, ok])

const program = Effect.gen(function* () {
  check('session is empty before login', Option.isNone(yield* auth.getSession))
  const login = yield* auth.login({ username: user, password })
  check('login signs in (or asks for TOTP)', login._tag === 'SignedIn' || login._tag === 'TotpRequired')
  const me = yield* auth.getSession
  check('session decodes after login', Option.isSome(me))
  const config = yield* auth.config
  check('auth config decodes', config.public_url.startsWith('http'))
  publicOrigin = config.public_url

  const list = yield* artifacts.list({ q: '', status: 'all' })
  check('artifact list decodes', true)
  // Tunnels have no files or versions to check, and e2e may have left the
  // newest artifact revoked; the report and revocation checks below need one
  // that is still live and has content.
  const first = list.find((a) => a.status === 'active' && a.kind !== 'tunnel') ?? list[0]
  if (first) {
    const a = yield* artifacts.get(first.id)
    const versions = yield* artifacts.versions(a.id)
    check('artifact detail + versions decode', versions.some((v) => v.current))
    check('files decode', (yield* artifacts.files(a.id, 0)).items.length > 0)
    yield* artifacts.views(a.id)
    check('access log decodes', true)
    check('preview link decodes', (yield* artifacts.preview(a.id)).url.includes('/__redrop/preview'))
    // Saving unchanged settings is idempotent (form -> request -> server).
    const values = Schema.decodeUnknownSync(ArtifactSettingsForm)(settingsDefaults(a, versions.find((v) => v.current)!.spa))
    const saved = yield* artifacts.update(a.id, toUpdateRequest(values, a.kind))
    check('unchanged settings round-trip', saved.title === a.title && saved.max_views === a.max_views && saved.url === a.url &&
      saved.info_button === a.info_button && saved.viewer_old_versions === a.viewer_old_versions && saved.feedback === a.feedback)
    // The viewer switches round-trip and can be put back.
    const flipped = yield* artifacts.update(a.id, toUpdateRequest({ ...values, infoButton: !a.info_button, oldVersions: !a.viewer_old_versions, feedback: !a.feedback }, a.kind))
    check('viewer settings round-trip', flipped.info_button === !a.info_button && flipped.viewer_old_versions === !a.viewer_old_versions && flipped.feedback === !a.feedback)
    yield* artifacts.update(a.id, { info_button: a.info_button, viewer_old_versions: a.viewer_old_versions, feedback: a.feedback })
    const comments = yield* artifacts.feedback(a.id, 'all')
    check('feedback list decodes', comments.every((c) => typeof c.body === 'string'))
    yield* artifacts.feedback(a.id, 'open')
  }

  const created = yield* tokens.create({ name: 'contract-check', ttl: '1d' })
  check('created token secret is Redacted', String(created.secret) !== Redacted.value(created.secret) && Redacted.value(created.secret).startsWith('rdp_'))
  check('token list decodes without secrets', (yield* tokens.list).every((t) => !('secret' in t)))
  yield* tokens.revoke(created.id)

  yield* account.identities
  check('identities decode', true)

  if (Option.isSome(me) && me.value.can_admin) {
    check('admin stats decode', (yield* admin.stats).users >= 1)
    check('admin users decode', (yield* admin.users).length >= 1)
    const inv = yield* admin.createInvite({ note: 'contract-check', role: 'user', ttl: '1d' })
    check('created invite URL is Redacted', Redacted.value(inv.url).includes('/invite/'))
    check('invite list decodes', (yield* admin.invites).some((i) => i.id === inv.id))
    yield* admin.deleteInvite(inv.id)
    // Abuse reports (DESIGN.md "Abuse handling"): file one as the report page
    // does, find it in the queue without a link on its artifact, dismiss it.
    // The marker is unique per run, so earlier runs on the same server don't
    // make the check expect a report that is already closed.
    if (first) {
      const link = (yield* artifacts.get(first.id)).url
      const marker = `contract-check-${Date.now()}`
      yield* reports.submit({ url: link, category: 'other', details: marker })
      const visitors = yield* admin.reports('open', 'visitor')
      check('report queue has filtered paging metadata', visitors.total >= 1 && visitors.page === 1 && visitors.page_size === 50 && visitors.items.every((r) => r.source === 'visitor' && r.status === 'open'))
      const queued = visitors.items.find((r) => r.artifact_id === first.id && r.details === marker)
      check('report reaches the admin queue', queued !== undefined && queued.reported_url === link && queued.artifact !== null && !('url' in queued.artifact))
      if (queued) {
        const dismissed = yield* admin.setReportStatus(queued.id, { status: 'dismissed' })
        check('report dismissal is audited', dismissed.status === 'dismissed' && (dismissed.history ?? []).some((e) => e.action === 'report.dismissed'))
      }
    }
    const reportQueue = yield* admin.reports('all')
    check('admin report queue decodes', Array.isArray(reportQueue.items) && reportQueue.total >= reportQueue.items.length)
    const beyondQueue = yield* admin.reports('all', 'all', Math.floor(reportQueue.total / reportQueue.page_size) + 2)
    check('report paging reaches an empty page without losing the total', beyondQueue.items.length === 0 && beyondQueue.total === reportQueue.total)
    const systemQueue = yield* admin.reports('all', 'system')
    check('system report filter excludes visitors', systemQueue.items.every((r) => r.source === 'system'))
    const all = yield* admin.artifacts('', 'all')
    check('admin artifacts decode without links', all.every((a) => !('url' in a)))
    const tunnels = yield* admin.tunnels
    check('admin tunnels decode without links', tunnels.every((t) => !('url' in t) && all.some((a) => a.id === t.id && a.online)))
    // An administrator's revocation: the owner sees it and cannot undo it.
    if (first && first.status === 'active') {
      const revoked = yield* admin.setArtifactRevoked(first.id, true)
      check('admin revocation is reported', revoked.status === 'revoked' && revoked.revoked_by_admin)
      check('owner sees the admin revocation', (yield* artifacts.get(first.id)).revoked_by_admin)
      const refused = yield* Effect.flip(artifacts.update(first.id, { revoked: false }))
      check('owner cannot undo an admin revocation', refused._tag === 'Forbidden')
      const restored = yield* admin.setArtifactRevoked(first.id, false)
      check('admin restore clears it', restored.status === 'active' && !restored.revoked_by_admin)
    }
  }

  yield* auth.logout
  check('session is empty after logout', Option.isNone(yield* auth.getSession))
})

const exit = await Effect.runPromiseExit(program.pipe(Effect.provide(Live)))
for (const [name, ok] of checks) console.log(`${ok ? '  ok  ' : 'FAIL  '}${name}`)
if (Exit.isFailure(exit)) {
  console.error('FAIL  ' + Cause.pretty(exit.cause))
  process.exit(1)
}
if (checks.some(([, ok]) => !ok)) process.exit(1)
console.log('CONTRACT CHECK PASSED')
