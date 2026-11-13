/**
 * Dashboard routes. The server redirects to or mints links for several of
 * these (internal/dashpaths); a Go test checks that each one appears here.
 */
export const paths = {
  home: '/',
  login: '/login',
  account: '/account',
  accountLinked: '/account?linked=1',
  tokens: '/tokens',
  tokensWelcome: '/tokens?welcome=1',
  artifacts: '/artifacts',
  artifact: (id: string) => `/artifacts/${encodeURIComponent(id)}`,
  admin: '/admin',
  adminUsers: '/admin/users',
  adminInvites: '/admin/invites',
  adminArtifacts: '/admin/artifacts',
  // One-time link prefixes (the secret token follows).
  setupPrefix: '/setup/',
  invitePrefix: '/invite/',
  resetPrefix: '/reset/',
} as const

/** Only same-origin, non-protocol-relative paths are valid post-login targets. */
export function safeNext(next: string | null | undefined): string {
  if (!next?.startsWith('/')) return paths.artifacts
  // URL parsing catches backslashes and embedded whitespace that browsers
  // normalize into a protocol-relative URL. Do not redirect back to login.
  const base = 'https://redrop.invalid'
  try {
    const url = new URL(next, base)
    if (url.origin !== base || url.pathname === paths.login) return paths.artifacts
    return url.pathname + url.search + url.hash
  } catch {
    return paths.artifacts
  }
}

export function loginPath(next?: string): string {
  return next && next !== paths.home ? `${paths.login}?next=${encodeURIComponent(next)}` : paths.login
}
