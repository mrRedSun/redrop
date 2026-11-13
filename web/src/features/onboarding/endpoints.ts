import { RedropApi, json } from '@/api/redrop-api'
import { Me } from '@/features/auth/schemas'
import { AcceptInviteRequest, InviteInfo, ResetInfo, ResetPasswordRequest, SetupRequest } from './schemas'

// One-time link endpoints. 401/410 here describe the link or the submitted
// credentials, not the session, so they are `credentials` calls.

export const setup = (req: typeof SetupRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['setup'], body: json(SetupRequest, req), response: Me, credentials: true }))

export const inviteInfo = (token: string) =>
  RedropApi.use((api) => api.call({ method: 'GET', path: ['invites', token], response: InviteInfo, credentials: true }))

export const acceptInvite = (token: string, req: typeof AcceptInviteRequest.Type) =>
  RedropApi.use((api) =>
    api.call({ method: 'POST', path: ['invites', token, 'accept'], body: json(AcceptInviteRequest, req), response: Me, credentials: true }),
  )

export const resetInfo = (token: string) =>
  RedropApi.use((api) => api.call({ method: 'GET', path: ['reset', token], response: ResetInfo, credentials: true }))

export const resetPassword = (token: string, req: typeof ResetPasswordRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['reset', token], body: json(ResetPasswordRequest, req), credentials: true }))
