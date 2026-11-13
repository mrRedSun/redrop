import { Schema } from 'effect'
import { ListOf, Timestamp } from '@/api/codecs'
import { NewPassword } from '@/features/onboarding/schemas'

// ---- wire ----

export const Identity = Schema.Struct({
  id: Schema.String,
  provider: Schema.String,
  label: Schema.String,
  email: Schema.String,
  created_at: Timestamp,
})
export type Identity = typeof Identity.Type
export const IdentityList = ListOf(Identity)

export const UpdateMeRequest = Schema.Struct({ display_name: Schema.String })
export const ChangePasswordRequest = Schema.Struct({ current_password: Schema.String, new_password: Schema.String })
export const TotpCodeRequest = Schema.Struct({ code: Schema.String })

/** Enrollment secret: shown once while enabling, never logged. */
export const TotpSetup = Schema.Struct({
  secret: Schema.RedactedFromValue(Schema.String),
  otpauth_url: Schema.RedactedFromValue(Schema.String),
})
export type TotpSetup = typeof TotpSetup.Type

// ---- forms ----

export const ProfileForm = Schema.Struct({
  displayName: Schema.Trim.check(Schema.isMaxLength(64, { message: 'At most 64 characters' })),
})
export type ProfileValues = typeof ProfileForm.Type

export const PasswordForm = Schema.Struct({ current: Schema.String, password: NewPassword, confirm: Schema.String }).check(
  Schema.makeFilter((v) => (v.password === v.confirm ? undefined : { path: ['confirm'], issue: 'Passwords do not match' })),
)
export type PasswordValues = typeof PasswordForm.Type
export const passwordDefaults: typeof PasswordForm.Encoded = { current: '', password: '', confirm: '' }

export const TotpCodeForm = Schema.Struct({
  code: Schema.Trim.check(Schema.isPattern(/^\d{6}$/, { message: 'Enter the 6-digit code' })),
})
export type TotpCodeValues = typeof TotpCodeForm.Type
export const totpCodeDefaults: typeof TotpCodeForm.Encoded = { code: '' }

// ---- form -> request (pure) ----

export const toUpdateMeRequest = (v: ProfileValues): typeof UpdateMeRequest.Type => ({ display_name: v.displayName })
export const toChangePasswordRequest = (v: PasswordValues): typeof ChangePasswordRequest.Type => ({
  current_password: v.current,
  new_password: v.password,
})
