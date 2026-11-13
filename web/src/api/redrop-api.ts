import { Context, Effect, Layer, Option, Schema, flow } from 'effect'
import { HttpClient, HttpClientRequest, HttpClientResponse } from 'effect/http'
import { Reactivity } from 'effect/reactivity'
import { type ApiError, fromProblem, offline } from './errors'
import { Ok } from './codecs'

/**
 * The only module that touches HTTP (oxlint no-restricted-imports fences
 * `effect/http` and this file to `src/features/* /endpoints.ts`).
 *
 * Owns, once: base path, CSRF header, timeout, request encoding, response
 * decoding, problem -> tagged error, and the "any 401 re-checks the
 * session" policy. Features describe a call; they never see a request,
 * a response, a status code, or a header.
 */

declare module 'effect/reactivity/Reactivity' {
  interface Keys {
    /** The current session (`GET /me`). Invalidated by any 401, login and logout. */
    readonly session: never
  }
}

/** Path segments under /api/v1. Each dynamic segment is URL-encoded by RedropApi, never by callers. */
export type ApiPath = readonly [string, ...ReadonlyArray<string | number>]

/** A JSON body paired with the schema that encodes it. Built with `json()`. */
export interface JsonBody {
  readonly encode: (req: HttpClientRequest.HttpClientRequest) => Effect.Effect<HttpClientRequest.HttpClientRequest>
}

export const json = <S extends Schema.Codec<any, any>>(schema: S, value: S['Type']): JsonBody => ({
  // Encoding a value that already satisfied its own Type is a bug, not a user error.
  encode: (req) => HttpClientRequest.schemaBodyJson(schema)(req, value).pipe(Effect.orDie),
})

export interface Call {
  readonly method: 'GET' | 'POST' | 'PATCH' | 'DELETE'
  readonly path: ApiPath
  /** Undefined and empty values are dropped. */
  readonly query?: Readonly<Record<string, string | undefined>>
  readonly body?: JsonBody
  /**
   * Endpoints where 401 is an answer about the submitted credentials, not a
   * lost session (login, setup, invite, reset). They skip the session re-check.
   */
  readonly credentials?: true
}

export interface RedropApiShape {
  /** Decodes a 2xx body with `response`. A body that does not match is a defect (contract drift), not an ApiError. */
  readonly call: {
    <A>(spec: Call & { readonly response: Schema.Decoder<A> }): Effect.Effect<A, ApiError>
    (spec: Call): Effect.Effect<void, ApiError>
  }
  /** Like `call`, but 401 means "no session": Option.none(), no re-check. Only the session query uses it. */
  readonly probe: <A>(spec: Call & { readonly response: Schema.Decoder<A> }) => Effect.Effect<Option.Option<A>, ApiError>
}

export class RedropApi extends Context.Service<RedropApi, RedropApiShape>()('redrop/RedropApi') {
  /** Requires HttpClient (BrowserHttpClient.layerFetch in the app, a stub in tests) and Reactivity (provided by Atom.runtime). */
  static readonly layer = Layer.effect(RedropApi, Effect.gen(function* () {
    const reactivity = yield* Reactivity.Reactivity
    const client = (yield* HttpClient.HttpClient).pipe(
      HttpClient.mapRequest(flow(
        HttpClientRequest.prependUrl('/api/v1'),
        HttpClientRequest.setHeader('X-Redrop-Request', '1'), // server CSRF check for cookie auth
        HttpClientRequest.acceptJson,
      )),
      HttpClient.timeout('30 seconds'),
    )

    const send = (spec: Call): Effect.Effect<HttpClientResponse.HttpClientResponse, ApiError> =>
      Effect.gen(function* () {
        const url = '/' + spec.path.map((s) => encodeURIComponent(String(s))).join('/')
        const urlParams = Object.entries(spec.query ?? {}).filter((e): e is [string, string] => !!e[1])
        let req = HttpClientRequest.make(spec.method)(url, { urlParams })
        if (spec.body) req = yield* spec.body.encode(req)
        const res = yield* client.execute(req).pipe(Effect.mapError(() => offline('Cannot reach the server')))
        if (res.status < 400) return res
        const problem = yield* HttpClientResponse.schemaBodyJson(ProblemBody)(res).pipe(
          Effect.orElseSucceed(() => ({ title: undefined, detail: undefined })), // proxy error page, empty body
        )
        return yield* Effect.fail(fromProblem(res.status, problem.title, problem.detail))
      })

    const decode = <A>(response: Schema.Decoder<A>) => (res: HttpClientResponse.HttpClientResponse) =>
      HttpClientResponse.schemaBodyJson(response)(res).pipe(
        // Parse errors may contain the raw body, including a one-time secret.
        Effect.catch(() => Effect.die(new Error('The server response did not match the API contract.'))),
      )

    function call<A>(spec: Call & { readonly response: Schema.Decoder<A> }): Effect.Effect<A, ApiError>
    function call(spec: Call): Effect.Effect<void, ApiError>
    function call(spec: Call & { readonly response?: Schema.Decoder<unknown> }): Effect.Effect<unknown, ApiError> {
      return send(spec).pipe(
        Effect.flatMap((res) => (spec.response ? decode(spec.response)(res) : decode(Ok)(res).pipe(Effect.asVoid))),
        // Any 401 may mean the session ended, or may be a wrong current
        // password: re-read the session instead of assuming sign-out.
        Effect.tapErrorTag('Unauthorized', () => (spec.credentials ? Effect.void : reactivity.invalidate(['session']))),
      )
    }

    const probe = <A>(spec: Call & { readonly response: Schema.Decoder<A> }) =>
      send(spec).pipe(
        Effect.flatMap(decode(spec.response)),
        Effect.map(Option.some),
        Effect.catchTag('Unauthorized', () => Effect.succeed(Option.none<A>())),
      )

    return RedropApi.of({ call, probe })
  }))
}

const ProblemBody = Schema.Struct({
  title: Schema.optionalKey(Schema.String),
  detail: Schema.optionalKey(Schema.String),
})
