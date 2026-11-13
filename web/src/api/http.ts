import createClient, { type Middleware } from 'openapi-fetch'
import type { paths } from './schema'

/** Error carrying RFC 9457 problem details returned by the server. */
export class ApiError extends Error {
  readonly status: number
  /** Machine-readable code (problem `title`), e.g. "totp_required". */
  readonly code: string

  constructor(status: number, code: string, detail: string) {
    super(detail || code || `Request failed (${status})`)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

const problemDetails: Middleware = {
  async onResponse({ response }) {
    if (response.ok) return undefined
    let code = ''
    let detail = ''
    try {
      const body = (await response.clone().json()) as { title?: string; detail?: string }
      code = body.title ?? ''
      detail = body.detail ?? ''
    } catch {
      // Not JSON (e.g. a proxy error page): fall back to the status text.
      detail = response.statusText
    }
    throw new ApiError(response.status, code, detail)
  },
}

/**
 * Typed client generated from the server's OpenAPI document.
 * Only feature `api.ts` modules should import this.
 */
export const http = createClient<paths>({
  baseUrl: '',
  credentials: 'same-origin',
  // Required by the server's CSRF protection for cookie-authenticated writes.
  headers: { 'X-Redrop-Request': '1' },
})
http.use(problemDetails)

/** Resolves an openapi-fetch result to its data; failures already threw. */
export async function data<T>(request: Promise<{ data?: T }>): Promise<T> {
  const { data } = await request
  return data as T
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

export function isUnauthorized(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401
}
