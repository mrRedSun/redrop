import { Option, Schema } from 'effect'
import { ListOf, Timestamp } from '@/api/codecs'

// ---- wire ----

export const ArtifactStatus = Schema.Literals(['active', 'expired', 'exhausted', 'revoked'])
export type ArtifactStatus = typeof ArtifactStatus.Type

export const Artifact = Schema.Struct({
  id: Schema.String,
  title: Schema.String,
  kind: Schema.Literals(['html', 'file', 'bundle']),
  status: ArtifactStatus,
  /** Secret link. Present for the owner only; admin views use AdminArtifact (no url field at all). */
  url: Schema.String,
  has_password: Schema.Boolean,
  max_views: Schema.Number,
  views: Schema.Number,
  previews: Schema.Number,
  files: Schema.Number,
  size: Schema.Number,
  total_size: Schema.Number,
  version: Schema.Number,
  versions: Schema.Number,
  token_name: Schema.optionalKey(Schema.String),
  created_at: Timestamp,
  updated_at: Timestamp,
  expires_at: Schema.NullOr(Timestamp),
  last_viewed_at: Schema.NullOr(Timestamp),
  purge_at: Schema.NullOr(Timestamp),
  /** Effective: the artifact's own setting, or the server default when it has none. */
  info_button: Schema.Boolean,
  /** Viewers can open older versions from the info panel. */
  viewer_old_versions: Schema.Boolean,
})
export type Artifact = typeof Artifact.Type
export const ArtifactList = ListOf(Artifact)

/** PATCH /artifacts/{id}. Absent key = unchanged. */
export const UpdateArtifactRequest = Schema.Struct({
  title: Schema.optionalKey(Schema.String),
  ttl: Schema.optionalKey(Schema.String),
  password: Schema.optionalKey(Schema.String),
  max_views: Schema.optionalKey(Schema.Number),
  reset_views: Schema.optionalKey(Schema.Boolean),
  spa: Schema.optionalKey(Schema.Boolean),
  info_button: Schema.optionalKey(Schema.Boolean),
  viewer_old_versions: Schema.optionalKey(Schema.Boolean),
  revoked: Schema.optionalKey(Schema.Boolean),
})
export type UpdateArtifactRequest = typeof UpdateArtifactRequest.Type

export const Version = Schema.Struct({
  number: Schema.Number,
  kind: Schema.String,
  size: Schema.Number,
  files: Schema.Number,
  entry: Schema.String,
  spa: Schema.Boolean,
  note: Schema.String,
  /** Viewers see the note in the info panel. False for notes written before notes were shown. */
  note_public: Schema.Boolean,
  created_at: Timestamp,
  token_name: Schema.optionalKey(Schema.String),
  current: Schema.Boolean,
})
export type Version = typeof Version.Type
export const VersionList = ListOf(Version)

export const FileEntry = Schema.Struct({ path: Schema.String, size: Schema.Number, content_type: Schema.String })
export type FileEntry = typeof FileEntry.Type
export const FileList = Schema.Struct({ version: Schema.Number, items: Schema.NullOr(Schema.Array(FileEntry)) })

export const View = Schema.Struct({
  at: Timestamp,
  outcome: Schema.String,
  country: Schema.String,
  client: Schema.String,
})
export type View = typeof View.Type
export const ViewList = ListOf(View)

export const PreviewLink = Schema.Struct({ url: Schema.String, expires_at: Timestamp })
export type PreviewLink = typeof PreviewLink.Type

// ---- list filter (lives in the URL; this schema is its only parser) ----

export const StatusFilter = Schema.Literals(['active', 'expired', 'exhausted', 'revoked', 'all'])
export const ArtifactFilter = Schema.Struct({ q: Schema.String, status: StatusFilter })
export type ArtifactFilter = typeof ArtifactFilter.Type
export const defaultFilter: ArtifactFilter = { q: '', status: 'all' }

/** URL -> filter. Unknown or missing values fall back per field; never throws. */
export const filterFromSearch = (params: URLSearchParams): ArtifactFilter => ({
  q: params.get('q')?.trim() ?? defaultFilter.q,
  status: Option.getOrElse(Schema.decodeUnknownOption(StatusFilter)(params.get('status')), () => defaultFilter.status),
})

// ---- settings form ----

export const TtlChoice = Schema.Literals(['keep', '1h', '1d', '7d', '30d', '1y', 'never'])
export const PasswordAction = Schema.Literals(['keep', 'set', 'remove'])

const ViewLimit = Schema.NumberFromString.check(
  Schema.isInt({ message: 'Whole number' }),
  Schema.isBetween({ minimum: 0, maximum: 1_000_000 }, { message: '0 to 1,000,000 (0 = unlimited)' }),
)

export const ArtifactSettingsForm = Schema.Struct({
  title: Schema.Trim.check(Schema.isMaxLength(200, { message: 'At most 200 characters' })),
  ttl: TtlChoice,
  passwordAction: PasswordAction,
  password: Schema.String,
  maxViews: ViewLimit,
  resetViews: Schema.Boolean,
  spa: Schema.Boolean,
  infoButton: Schema.Boolean,
  oldVersions: Schema.Boolean,
}).check(
  Schema.makeFilter((v) =>
    v.passwordAction !== 'set' || v.password.length > 0 ? undefined : { path: ['password'], issue: 'Enter the new password' },
  ),
)
export type ArtifactSettings = typeof ArtifactSettingsForm.Type
export type ArtifactSettingsInput = typeof ArtifactSettingsForm.Encoded

/**
 * Artifact -> form defaults. Pure. The read model has no SPA flag; it is a
 * property of the current version, so callers pass that version's value.
 */
export const settingsDefaults = (a: Artifact, spa: boolean): ArtifactSettingsInput => ({
  title: a.title,
  ttl: 'keep',
  passwordAction: 'keep',
  password: '',
  maxViews: String(a.max_views),
  resetViews: false,
  spa,
  infoButton: a.info_button,
  oldVersions: a.viewer_old_versions,
})

/**
 * Form -> PATCH body. Keep omits expiry/password instead of resetting
 * them. Explicit TTL choices are relative to the submission time. `spa`
 * only applies to bundles. The viewer switches are always sent, so saving
 * stores the values the owner saw as the artifact's own.
 */
export const toUpdateRequest = (v: ArtifactSettings, kind: Artifact['kind']): UpdateArtifactRequest => ({
  title: v.title,
  max_views: v.maxViews,
  ...(v.ttl !== 'keep' && { ttl: v.ttl }),
  ...(v.passwordAction === 'set' && { password: v.password }),
  ...(v.passwordAction === 'remove' && { password: '' }),
  ...(v.resetViews && { reset_views: true }),
  ...(kind === 'bundle' && { spa: v.spa }),
  info_button: v.infoButton,
  viewer_old_versions: v.oldVersions,
})
