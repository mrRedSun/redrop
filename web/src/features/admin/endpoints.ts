import { Effect } from 'effect'
import { RedropApi, json } from '@/api/redrop-api'
import {
  AdminArtifactList,
  AdminArtifactUpdate,
  AdminArtifact,
  AdminResetRequest,
  AdminUpdateUserRequest,
  CreateInviteRequest,
  CreatedInvite,
  InviteList,
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

export const artifacts = (q: string, status: string) =>
  RedropApi.use((api) => api.call({ method: 'GET', path: ['admin', 'artifacts'], query: { q, status }, response: AdminArtifactList })).pipe(
    Effect.map(items),
  )

export const setArtifactRevoked = (id: string, revoked: boolean) =>
  RedropApi.use((api) =>
    api.call({ method: 'PATCH', path: ['admin', 'artifacts', id], body: json(AdminArtifactUpdate, { revoked }), response: AdminArtifact }),
  )

export const deleteArtifact = (id: string) => RedropApi.use((api) => api.call({ method: 'DELETE', path: ['admin', 'artifacts', id] }))
