import { Effect } from 'effect'
import { ProblemCode, type ApiError } from '@/api/errors'
import { RedropApi, json } from '@/api/redrop-api'
import { AuthConfig, LoginRequest, Me, type LoginOutcome } from './schemas'

/** Typed endpoint functions: Schemas in, domain out. The only auth module that imports RedropApi. */

/** The current session; none when signed out. */
export const getSession = RedropApi.use((api) => api.probe({ method: 'GET', path: ['me'], response: Me }))

export const login = (req: LoginRequest): Effect.Effect<LoginOutcome, ApiError, RedropApi> =>
  RedropApi.use((api) =>
    api.call({ method: 'POST', path: ['auth', 'login'], body: json(LoginRequest, req), response: Me, credentials: true }),
  ).pipe(
    Effect.map((me): LoginOutcome => ({ _tag: 'SignedIn', me })),
    Effect.catchIf(
      (e) => e._tag === 'Unauthorized' && e.code === ProblemCode.totpRequired,
      (): Effect.Effect<LoginOutcome> => Effect.succeed({ _tag: 'TotpRequired' }),
    ),
  )

export const config = RedropApi.use((api) => api.call({ method: 'GET', path: ['auth', 'config'], response: AuthConfig }))

export const logout = RedropApi.use((api) => api.call({ method: 'POST', path: ['auth', 'logout'] }))

/** Browser navigation target that starts single sign-on (a redirect, not a fetch). */
export const ssoStartUrl = (provider: string, mode: 'login' | 'link' | 'invite', inviteToken?: string): string => {
  const q = new URLSearchParams({ mode })
  if (inviteToken) q.set('token', inviteToken)
  return `/api/v1/auth/oidc/${encodeURIComponent(provider)}/start?${q}`
}
