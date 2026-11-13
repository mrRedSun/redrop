import { Cause, Schema, Predicate } from 'effect'

/**
 * Failures every RedropApi call can produce. Decoded once from the server's
 * RFC 9457 problem details (status -> tag, `title` -> code, `detail` ->
 * message). Pure module: no HTTP here, so components may import
 * `describeError` without crossing the HTTP fence.
 */
interface Problem {
  readonly status: number
  /** Machine-readable problem `title`, e.g. "totp_required"; status text when the server set none. */
  readonly code: string
  /** Human-readable, user-safe `detail`. */
  readonly message: string
}

const ProblemFields = { status: Schema.Number, code: Schema.String, message: Schema.String }

export class Unauthorized extends Schema.TaggedError<Unauthorized>()('Unauthorized', ProblemFields) {}
export class Forbidden extends Schema.TaggedError<Forbidden>()('Forbidden', ProblemFields) {}
export class NotFound extends Schema.TaggedError<NotFound>()('NotFound', ProblemFields) {}
export class Conflict extends Schema.TaggedError<Conflict>()('Conflict', ProblemFields) {}
export class Invalid extends Schema.TaggedError<Invalid>()('Invalid', ProblemFields) {}
export class Gone extends Schema.TaggedError<Gone>()('Gone', ProblemFields) {}
export class TooLarge extends Schema.TaggedError<TooLarge>()('TooLarge', ProblemFields) {}
export class RateLimited extends Schema.TaggedError<RateLimited>()('RateLimited', ProblemFields) {}
export class ServerError extends Schema.TaggedError<ServerError>()('ServerError', ProblemFields) {}
export class Offline extends Schema.TaggedError<Offline>()('Offline', ProblemFields) {}

export type ApiError =
  | Unauthorized | Forbidden | NotFound | Conflict | Invalid | Gone | TooLarge | RateLimited | ServerError | Offline

/** Problem codes the dashboard branches on. One list, next to the decoder. */
export const ProblemCode = { totpRequired: 'totp_required' } as const

/** The single status -> tagged error mapping (mirror of httpapi.StatusOf). */
export function fromProblem(status: number, title: string | undefined, detail: string | undefined): ApiError {
  const p: Problem = { status, code: title ?? '', message: detail || title || `Request failed (${status})` }
  switch (status) {
    case 400: case 422: return new Invalid(p)
    case 401: return new Unauthorized(p)
    case 403: return new Forbidden(p)
    case 404: return new NotFound(p)
    case 409: return new Conflict(p)
    case 410: return new Gone(p)
    case 413: return new TooLarge(p)
    case 429: return new RateLimited(p)
    default: return new ServerError(p)
  }
}

export const offline = (message: string): ApiError => new Offline({ status: 0, code: 'network', message })

/** User-facing text for any failure cause (typed error, defect, interrupt). */
export function describeCause(cause: Cause.Cause<unknown>): string {
  return describeError(Cause.squash(cause))
}

/** True for a typed API failure with the given tag (e.g. 'NotFound'), for UI branching. */
export const isApiError = (error: unknown, tag: ApiError['_tag']): boolean => Predicate.isTagged(error, tag)

export function describeError(error: unknown): string {
  if (Predicate.isTagged(error, 'Offline')) return 'Cannot reach the server. Check your connection and try again.'
  if (Predicate.hasProperty(error, '_tag') && Predicate.hasProperty(error, 'message') && Predicate.isString(error.message)) {
    return error.message
  }
  return 'Something went wrong. Reload the page and try again.'
}
