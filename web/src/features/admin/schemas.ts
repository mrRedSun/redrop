import { Option, Schema } from 'effect'
import { ListOf, Timestamp } from '@/api/codecs'
import { ArtifactStatus } from '@/features/artifacts/schemas'
import { User } from '@/features/auth/schemas'
import { GB } from '@/lib/format'

// ---- wire ----

export const Stats = Schema.Struct({
  users: Schema.Number,
  artifacts: Schema.Number,
  active_artifacts: Schema.Number,
  logical_bytes: Schema.Number,
  stored_bytes: Schema.Number,
  max_total_bytes: Schema.Number,
  max_version_bytes: Schema.Number,
})
export type Stats = typeof Stats.Type

export { User }
export type User = typeof User.Type
export const UserList = ListOf(User)

/**
 * Admins see metadata only (DESIGN.md "Admin privacy"). This Schema has no
 * `url`: even if the server sent one, decoding would drop it.
 */
export const AdminArtifact = Schema.Struct({
  id: Schema.String,
  title: Schema.String,
  kind: Schema.String,
  status: ArtifactStatus,
  owner_username: Schema.String,
  has_password: Schema.Boolean,
  max_views: Schema.Number,
  views: Schema.Number,
  size: Schema.Number,
  total_size: Schema.Number,
  versions: Schema.Number,
  created_at: Timestamp,
  expires_at: Schema.NullOr(Timestamp),
  last_viewed_at: Schema.NullOr(Timestamp),
})
export type AdminArtifact = typeof AdminArtifact.Type
export const AdminArtifactList = ListOf(AdminArtifact)

export const Invite = Schema.Struct({
  id: Schema.String,
  note: Schema.String,
  role: Schema.String,
  quota_bytes: Schema.Number,
  created_at: Timestamp,
  expires_at: Timestamp,
  used_at: Schema.NullOr(Timestamp),
  used_by: Schema.optionalKey(Schema.String),
})
export type Invite = typeof Invite.Type
export const InviteList = ListOf(Invite)

/** Returned only at creation; the URL carries the one-time secret. */
export const CreatedInvite = Schema.Struct({ ...Invite.fields, url: Schema.RedactedFromValue(Schema.String) })
export type CreatedInvite = typeof CreatedInvite.Type

export const ResetLink = Schema.Struct({ url: Schema.RedactedFromValue(Schema.String), expires_at: Timestamp })
export type ResetLink = typeof ResetLink.Type

export const AdminUpdateUserRequest = Schema.Struct({
  quota_bytes: Schema.optionalKey(Schema.Number),
  disabled: Schema.optionalKey(Schema.Boolean),
  role: Schema.optionalKey(Schema.Literals(['admin', 'user'])),
  display_name: Schema.optionalKey(Schema.String),
})
export const AdminResetRequest = Schema.Struct({ clear_totp: Schema.Boolean })
export const CreateInviteRequest = Schema.Struct({
  note: Schema.String,
  role: Schema.optionalKey(Schema.Literals(['admin', 'user'])),
  quota_bytes: Schema.optionalKey(Schema.Number),
  ttl: Schema.optionalKey(Schema.String),
})
export const AdminArtifactUpdate = Schema.Struct({ revoked: Schema.Boolean })

// ---- forms ----

const QuotaGB = Schema.NumberFromString.check(Schema.isGreaterThanOrEqualTo(0, { message: '0 or more (0 = only the server total applies)' }))
const Role = Schema.Literals(['admin', 'user'])

export const EditUserForm = Schema.Struct({
  displayName: Schema.Trim.check(Schema.isMaxLength(64, { message: 'At most 64 characters' })),
  role: Role,
  quotaGB: QuotaGB,
  disabled: Schema.Boolean,
})
export type EditUserValues = typeof EditUserForm.Type
export const editUserDefaults = (u: User): typeof EditUserForm.Encoded => ({
  displayName: u.display_name,
  role: u.role,
  quotaGB: String(u.quota_bytes / GB),
  disabled: u.disabled,
})
/** Absolute values only, so re-sending changes nothing. */
export const toUpdateUserRequest = (v: EditUserValues): typeof AdminUpdateUserRequest.Type => ({
  display_name: v.displayName,
  role: v.role,
  quota_bytes: Math.round(v.quotaGB * GB),
  disabled: v.disabled,
})

export const InviteTtl = Schema.Literals(['1d', '7d', '30d'])
export const CreateInviteForm = Schema.Struct({
  note: Schema.Trim.check(Schema.isMaxLength(200, { message: 'At most 200 characters' })),
  role: Role,
  /** Empty = the server's default quota. */
  quotaGB: Schema.Trim,
  ttl: InviteTtl,
}).check(
  Schema.makeFilter((v) =>
    v.quotaGB === '' || Option.isSome(Schema.decodeUnknownOption(QuotaGB)(v.quotaGB)) ? undefined : { path: ['quotaGB'], issue: 'A number of GB, or empty for the default' },
  ),
)
export type CreateInviteValues = typeof CreateInviteForm.Type
export const createInviteDefaults: typeof CreateInviteForm.Encoded = { note: '', role: 'user', quotaGB: '', ttl: '7d' }
export const toCreateInviteRequest = (v: CreateInviteValues): typeof CreateInviteRequest.Type => ({
  note: v.note,
  role: v.role,
  ttl: v.ttl,
  ...(v.quotaGB !== '' && { quota_bytes: Math.round(Number(v.quotaGB) * GB) }),
})

export const ResetLinkForm = Schema.Struct({ clearTotp: Schema.Boolean })
export type ResetLinkValues = typeof ResetLinkForm.Type
