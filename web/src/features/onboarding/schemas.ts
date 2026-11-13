import { Schema } from 'effect'
import { Timestamp } from '@/api/codecs'
import { AuthProvider } from '@/features/auth/schemas'

// ---- wire ----

export const InviteInfo = Schema.Struct({
  valid: Schema.Boolean,
  role: Schema.String,
  expires_at: Timestamp,
  providers: Schema.NullOr(Schema.Array(AuthProvider)),
})
export type InviteInfo = typeof InviteInfo.Type

export const ResetInfo = Schema.Struct({ valid: Schema.Boolean, username: Schema.String })
export type ResetInfo = typeof ResetInfo.Type

export const SetupRequest = Schema.Struct({ token: Schema.String, username: Schema.String, password: Schema.String })
export const AcceptInviteRequest = Schema.Struct({ username: Schema.String, password: Schema.String })
export const ResetPasswordRequest = Schema.Struct({ password: Schema.String })

// ---- forms (rules mirror the server; the server stays authoritative) ----

/** Same rule as the server's username check. */
export const Username = Schema.Trim.check(
  Schema.isPattern(/^[a-z0-9][a-z0-9._-]{1,31}$/, { message: '2-32 characters: lowercase letters, digits, dot, dash, underscore' }),
)

/** Same rule as security.ValidatePassword. */
export const NewPassword = Schema.String.check(
  Schema.isMinLength(10, { message: 'At least 10 characters' }),
  Schema.isMaxLength(256, { message: 'At most 256 characters' }),
)

const passwordsMatch = Schema.makeFilter((v: { password: string; confirm: string }) =>
  v.password === v.confirm ? undefined : { path: ['confirm'], issue: 'Passwords do not match' },
)

export const NewAccountForm = Schema.Struct({ username: Username, password: NewPassword, confirm: Schema.String }).check(passwordsMatch)
export type NewAccount = typeof NewAccountForm.Type
export const newAccountDefaults: typeof NewAccountForm.Encoded = { username: '', password: '', confirm: '' }

export const NewPasswordForm = Schema.Struct({ password: NewPassword, confirm: Schema.String }).check(passwordsMatch)
export type NewPasswordValues = typeof NewPasswordForm.Type
export const newPasswordDefaults: typeof NewPasswordForm.Encoded = { password: '', confirm: '' }
