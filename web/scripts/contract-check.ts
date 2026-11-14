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
import * as tokens from '@/features/tokens/endpoints'
import { Schema } from 'effect'

const base = process.env.BASE ?? 'http://127.0.0.1:8080'
const user = process.env.E2E_USER ?? 'admin'
const password = process.env.E2E_PASSWORD ?? 'correct-horse-battery'

// HttpClient resolves relative URLs against `location` in the browser.
;(globalThis as { location?: unknown }).location = { origin: base, pathname: '/', href: base + '/' }

// fetch with a cookie jar and the browser's same-origin header, like the dashboard.
let cookie = ''
const browserFetch: typeof fetch = async (input, init) => {
  const headers = new Headers(init?.headers)
  if (cookie) headers.set('cookie', cookie)
  headers.set('sec-fetch-site', 'same-origin')
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
  check('auth config decodes', (yield* auth.config).public_url.startsWith('http'))

  const list = yield* artifacts.list({ q: '', status: 'all' })
  check('artifact list decodes', true)
  const first = list[0]
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
      saved.info_button === a.info_button && saved.viewer_old_versions === a.viewer_old_versions)
    // Both viewer switches round-trip and can be put back.
    const flipped = yield* artifacts.update(a.id, toUpdateRequest({ ...values, infoButton: !a.info_button, oldVersions: !a.viewer_old_versions }, a.kind))
    check('viewer settings round-trip', flipped.info_button === !a.info_button && flipped.viewer_old_versions === !a.viewer_old_versions)
    yield* artifacts.update(a.id, { info_button: a.info_button, viewer_old_versions: a.viewer_old_versions })
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
    const all = yield* admin.artifacts('', 'all')
    check('admin artifacts decode without links', all.every((a) => !('url' in a)))
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
