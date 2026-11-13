import { Effect } from 'effect'
import { RedropApi, json } from '@/api/redrop-api'
import { Me } from '@/features/auth/schemas'
import { ChangePasswordRequest, IdentityList, TotpCodeRequest, TotpSetup, UpdateMeRequest } from './schemas'

export const updateProfile = (req: typeof UpdateMeRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'PATCH', path: ['me'], body: json(UpdateMeRequest, req), response: Me }))

/** A 401 rechecks me: wrong current password must not itself sign the user out. */
export const changePassword = (req: typeof ChangePasswordRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['me', 'password'], body: json(ChangePasswordRequest, req) }))

export const beginTotp = RedropApi.use((api) => api.call({ method: 'POST', path: ['me', 'totp', 'setup'], response: TotpSetup }))

export const enableTotp = (code: string) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['me', 'totp', 'enable'], body: json(TotpCodeRequest, { code }) }))

export const disableTotp = (code: string) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['me', 'totp', 'disable'], body: json(TotpCodeRequest, { code }) }))

export const identities = RedropApi.use((api) => api.call({ method: 'GET', path: ['me', 'identities'], response: IdentityList })).pipe(
  Effect.map((r) => r.items ?? []),
)

export const unlinkIdentity = (id: string) => RedropApi.use((api) => api.call({ method: 'DELETE', path: ['me', 'identities', id] }))
