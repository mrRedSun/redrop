import { Schema } from 'effect'
import { ListOf, Timestamp } from '@/api/codecs'

export const Token = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  prefix: Schema.String,
  artifacts: Schema.Number,
  created_at: Timestamp,
  expires_at: Schema.NullOr(Timestamp),
  last_used_at: Schema.NullOr(Timestamp),
})
export const TokenList = ListOf(Token)

export const CreateTokenRequest = Schema.Struct({ name: Schema.String, ttl: Schema.optionalKey(Schema.String) })

/** The secret decodes straight into Redacted: it prints as <redacted> in logs, toasts and devtools. */
export const CreatedToken = Schema.Struct({ ...Token.fields, secret: Schema.RedactedFromValue(Schema.String) })
export type CreatedToken = typeof CreatedToken.Type
export type Token = typeof Token.Type

export const CreateTokenForm = Schema.Struct({
  name: Schema.Trim.check(Schema.isMinLength(1, { message: 'Name the token (e.g. the machine it runs on)' })),
  ttl: Schema.Literals(['30d', '90d', '1y', 'never']),
})
export type CreateTokenValues = typeof CreateTokenForm.Type
export const createTokenDefaults: typeof CreateTokenForm.Encoded = { name: '', ttl: '90d' }

/** Form -> request. Pure. */
export const toCreateTokenRequest = (v: CreateTokenValues): typeof CreateTokenRequest.Type => ({ name: v.name, ttl: v.ttl })
