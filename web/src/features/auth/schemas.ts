import { Schema } from 'effect'
import { Timestamp } from '@/api/codecs'

// ---- wire (mirror of web/openapi.json; hand-written, checked by contract test) ----

export const User = Schema.Struct({
  id: Schema.String,
  username: Schema.String,
  display_name: Schema.String,
  role: Schema.Literals(['admin', 'user']),
  disabled: Schema.Boolean,
  totp_enabled: Schema.Boolean,
  quota_bytes: Schema.Number,
  used_bytes: Schema.Number,
  artifacts: Schema.Number,
  created_at: Timestamp,
})

export const Me = Schema.Struct({
  user: User,
  can_admin: Schema.Boolean,
  auth_method: Schema.String,
  token_name: Schema.optionalKey(Schema.String),
})
export type Me = typeof Me.Type

export const LoginRequest = Schema.Struct({
  username: Schema.String,
  password: Schema.String,
  totp: Schema.optionalKey(Schema.String),
})
export type LoginRequest = typeof LoginRequest.Type

// ---- domain ----

/** A password login either signs in or asks for the authenticator code. */
export type LoginOutcome =
  | { readonly _tag: 'SignedIn'; readonly me: Me }
  | { readonly _tag: 'TotpRequired' }

// ---- forms (Encoded = what inputs hold, Type = what the mutation receives) ----

export const CredentialsForm = Schema.Struct({
  username: Schema.Trim.check(Schema.isMinLength(1, { message: 'Enter your username' })),
  password: Schema.String.check(Schema.isMinLength(1, { message: 'Enter your password' })),
})
export type Credentials = typeof CredentialsForm.Type
export const credentialsDefaults: typeof CredentialsForm.Encoded = { username: '', password: '' }

export const TotpForm = Schema.Struct({
  code: Schema.Trim.check(Schema.isPattern(/^\d{6}$/, { message: 'Enter the 6-digit code' })),
})
export const totpDefaults: typeof TotpForm.Encoded = { code: '' }

/** Form values -> request. Pure; the only place that knows `totp` is a field of LoginRequest. */
export const toLoginRequest = (c: Credentials, totp?: string): LoginRequest =>
  totp === undefined ? { username: c.username, password: c.password } : { ...c, totp }

export const AuthProvider = Schema.Struct({ id: Schema.String, label: Schema.String })
export type AuthProvider = typeof AuthProvider.Type

export const AuthConfig = Schema.Struct({
  version: Schema.String,
  setup_required: Schema.Boolean,
  password_login: Schema.Boolean,
  providers: Schema.NullOr(Schema.Array(AuthProvider)),
  admin_requires_sso: Schema.Boolean,
  public_url: Schema.String,
  content_url: Schema.String,
})
export type AuthConfig = typeof AuthConfig.Type
