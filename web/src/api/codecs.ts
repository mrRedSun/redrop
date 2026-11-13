import { Schema } from 'effect'

/** Shared wire codecs. Feature schemas compose these; nothing else lives here. */

/** RFC 3339 timestamp on the wire, `Date` in the app. */
export const Timestamp = Schema.DateFromString

/** Go encodes empty slices as null; the app always sees an array. */
export const ListOf = <S extends Schema.Top>(item: S) =>
  Schema.Struct({ items: Schema.NullOr(Schema.Array(item)) })

/** Successful commands without a payload still have a validated response. */
export const Ok = Schema.Struct({ ok: Schema.Boolean })
