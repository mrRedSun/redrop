import { Atom } from 'effect/reactivity'
import { mutation, query } from '@/runtime/runtime'
import * as endpoints from './endpoints'
import { toCreateInviteRequest, toUpdateUserRequest, type AdminArtifact, type CreateInviteValues, type EditUserValues, type ReportFilter, type ReportStatus, type ResetLinkValues } from './schemas'

declare module 'effect/reactivity/Reactivity' {
  interface Keys {
    /** Every admin read (stats, users, invites, all-artifact metadata, reports). */
    readonly admin: never
  }
}

export const stats = query(['admin'], endpoints.stats)
export const users = query(['admin'], endpoints.users)
export const invites = query(['admin'], endpoints.invites)

/** Key: `${status}|${q}`. */
export const artifacts = Atom.family((key: string) => {
  const [status, ...q] = key.split('|')
  return query(['admin'], endpoints.artifacts(q.join('|'), status))
})
export const artifactsKey = (status: string, q: string) => `${status}|${q}`

export const updateUser = Atom.family((id: string) =>
  mutation((v: EditUserValues) => endpoints.updateUser(id, toUpdateUserRequest(v)), {
    invalidates: ['admin', 'session'],
    success: 'User updated',
    failures: 'form',
  }),
)

/** The reset URL is a one-time secret: kept only while its dialog is open. */
export const resetLink = Atom.family((id: string) =>
  mutation((v: ResetLinkValues) => endpoints.resetLink(id, { clear_totp: v.clearTotp }), { invalidates: [], failures: 'form' }).pipe(Atom.setIdleTTL(0)),
)

/** The invite URL is a one-time secret: kept only while its dialog is open. */
export const createInvite = mutation((v: CreateInviteValues) => endpoints.createInvite(toCreateInviteRequest(v)), {
  invalidates: ['admin'],
  failures: 'form',
}).pipe(Atom.setIdleTTL(0))

export const deleteInvite = Atom.family((id: string) =>
  mutation((_: void) => endpoints.deleteInvite(id), { invalidates: ['admin'], success: 'Invite revoked' }),
)

export const setArtifactRevoked = Atom.family((id: string) =>
  mutation((revoked: boolean) => endpoints.setArtifactRevoked(id, revoked), {
    invalidates: ['admin', 'artifacts'],
    success: (a: AdminArtifact) => (a.status === 'revoked' ? 'Artifact revoked' : 'Artifact restored'),
  }),
)

export const deleteArtifact = Atom.family((id: string) =>
  mutation((_: void) => endpoints.deleteArtifact(id), { invalidates: ['admin', 'artifacts'], success: 'Artifact deleted' }),
)

export const endTunnel = Atom.family((id: string) =>
  mutation((_: void) => endpoints.endTunnel(id), { invalidates: ['admin', 'artifacts'], success: 'Tunnel ended; revoke it to keep it off' }),
)

/** Key: `${status}|${source}|${page}`. Only scalar keys enter the atom family. */
export const reports = Atom.family((key: string) => {
  const [status, source, page] = key.split('|')
  return query(['admin'], endpoints.reports(status as ReportFilter['status'], source as ReportFilter['source'], Number(page)))
})
export const reportsKey = (filter: ReportFilter) => `${filter.status}|${filter.source}|${filter.page}`

const reportDone: Record<ReportStatus, string> = { open: 'Report reopened', resolved: 'Report resolved', dismissed: 'Report dismissed' }

export const setReportStatus = Atom.family((id: string) =>
  mutation((status: ReportStatus) => endpoints.setReportStatus(id, { status }), { invalidates: ['admin'], success: (_, status) => reportDone[status] }),
)

export const revokeReported = Atom.family((id: string) =>
  mutation((_: void) => endpoints.revokeReported(id), { invalidates: ['admin', 'artifacts'], success: 'Artifact revoked' }),
)

export const suspendReportedOwner = Atom.family((id: string) =>
  mutation((_: void) => endpoints.suspendReportedOwner(id), { invalidates: ['admin', 'artifacts'], success: 'Owner suspended' }),
)
