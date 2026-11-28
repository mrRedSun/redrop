import { Effect } from 'effect'
import { RedropApi, json } from '@/api/redrop-api'
import {
  AdminArtifactList,
  AdminArtifactUpdate,
  AdminTunnelList,
  AdminArtifact,
  AdminResetRequest,
  AdminUpdateUserRequest,
  CreateInviteRequest,
  CreatedInvite,
  Invite,
  InviteList,
  Report,
  ReportList,
  ReportStatusUpdate,
  type ReportStatusFilter,
  type ReportSourceFilter,
  ResetLink,
  Stats,
  User,
  UserList,
} from './schemas'

const items = <A>(r: { readonly items: ReadonlyArray<A> | null }) => r.items ?? []

export const stats = RedropApi.use((api) => api.call({ method: 'GET', path: ['admin', 'stats'], response: Stats }))

export const users = RedropApi.use((api) => api.call({ method: 'GET', path: ['admin', 'users'], response: UserList })).pipe(Effect.map(items))

export const updateUser = (id: string, req: typeof AdminUpdateUserRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'PATCH', path: ['admin', 'users', id], body: json(AdminUpdateUserRequest, req), response: User }))

export const resetLink = (id: string, req: typeof AdminResetRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['admin', 'users', id, 'reset'], body: json(AdminResetRequest, req), response: ResetLink }))

export const invites = RedropApi.use((api) => api.call({ method: 'GET', path: ['admin', 'invites'], response: InviteList })).pipe(Effect.map(items))

export const createInvite = (req: typeof CreateInviteRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['admin', 'invites'], body: json(CreateInviteRequest, req), response: CreatedInvite }))

export const deleteInvite = (id: string) => RedropApi.use((api) => api.call({ method: 'DELETE', path: ['admin', 'invites', id] }))

/** Queues the invite's email again: the same one-time link, as first rendered. */
export const resendInvite = (id: string) => RedropApi.use((api) => api.call({ method: 'POST', path: ['admin', 'invites', id, 'resend'], response: Invite }))

export const artifacts = (q: string, status: string) =>
  RedropApi.use((api) => api.call({ method: 'GET', path: ['admin', 'artifacts'], query: { q, status }, response: AdminArtifactList })).pipe(
    Effect.map(items),
  )

export const setArtifactRevoked = (id: string, revoked: boolean) =>
  RedropApi.use((api) =>
    api.call({ method: 'PATCH', path: ['admin', 'artifacts', id], body: json(AdminArtifactUpdate, { revoked }), response: AdminArtifact }),
  )

export const deleteArtifact = (id: string) => RedropApi.use((api) => api.call({ method: 'DELETE', path: ['admin', 'artifacts', id] }))

export const tunnels = RedropApi.use((api) => api.call({ method: 'GET', path: ['admin', 'tunnels'], response: AdminTunnelList })).pipe(Effect.map(items))

/** Ends a tunnel's connection; the owner may start it again (revoke keeps it off). */
export const endTunnel = (id: string) => RedropApi.use((api) => api.call({ method: 'DELETE', path: ['admin', 'tunnels', id] }))

export const reports = (status: ReportStatusFilter, source: ReportSourceFilter = 'all', page = 1) =>
  RedropApi.use((api) => api.call({ method: 'GET', path: ['admin', 'reports'], query: { status, source, page: String(page) }, response: ReportList })).pipe(
    Effect.map((result) => ({ ...result, items: items(result) })),
  )

export const setReportStatus = (id: string, req: typeof ReportStatusUpdate.Type) =>
  RedropApi.use((api) => api.call({ method: 'PATCH', path: ['admin', 'reports', id], body: json(ReportStatusUpdate, req), response: Report }))

/** The administrator's revocation of the reported artifact; resolves its open reports. */
export const revokeReported = (id: string) => RedropApi.use((api) => api.call({ method: 'POST', path: ['admin', 'reports', id, 'revoke'], response: Report }))

/** Disables the owner's account and revokes all of its artifacts; resolves their open reports. */
export const suspendReportedOwner = (id: string) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['admin', 'reports', id, 'suspend'], response: Report }))
