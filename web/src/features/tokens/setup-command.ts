/**
 * One-line CLI setup: install the CLI, then log in with a new token.
 * Pure string building; the installers (internal/web/install) read
 * REDROP_TOKEN, pass it to `redrop login --token-stdin`, and clear it.
 */

export type SetupOs = 'unix' | 'windows'

/** POSIX sh single quotes: nothing inside is special, and ' becomes '\''. */
export const shQuote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'"

/**
 * PowerShell single quotes: nothing inside is expanded, and a quote is
 * escaped by doubling it. PowerShell also treats the typographic quotes
 * U+2018..U+201B as single quotes, so those are doubled too.
 */
export const psQuote = (value: string) => "'" + value.replace(/['‘’‚‛]/g, '$&$&') + "'"

/**
 * macOS/Linux: the variable is set only for `sh`, so `curl` never sees it.
 * Windows: the installer removes Env:REDROP_TOKEN when it finishes, on
 * success or failure, so it cannot shadow the saved login afterwards.
 */
export const setupCommand = (os: SetupOs, publicUrl: string, token: string) =>
  os === 'windows'
    ? `$env:REDROP_TOKEN=${psQuote(token)}; irm ${psQuote(publicUrl + '/install.ps1')} | iex`
    : `curl -fsSL ${shQuote(publicUrl + '/install.sh')} | REDROP_TOKEN=${shQuote(token)} sh`

/** Default OS tab from the browser's platform string (e.g. navigator.platform). */
export const setupOsFromPlatform = (platform: string): SetupOs => (/^win/i.test(platform.trim()) ? 'windows' : 'unix')
