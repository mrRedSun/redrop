export function formatBytes(n: number): string {
  if (!Number.isFinite(n)) return '—'
  if (n < 1000) return `${n} B`
  const units = ['kB', 'MB', 'GB', 'TB']
  let v = n
  let i = -1
  do {
    v /= 1000
    i++
  } while (v >= 1000 && i < units.length - 1)
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })

export function relativeTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const diff = new Date(iso).getTime() - Date.now()
  const abs = Math.abs(diff)
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ['year', 365 * 864e5],
    ['month', 30 * 864e5],
    ['week', 7 * 864e5],
    ['day', 864e5],
    ['hour', 36e5],
    ['minute', 6e4],
  ]
  for (const [unit, ms] of units) {
    if (abs >= ms) return rtf.format(Math.round(diff / ms), unit)
  }
  return diff >= 0 ? 'in a moment' : 'just now'
}

export function formatDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

export function generatePassword(): string {
  const alphabet = 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789'
  const bytes = new Uint32Array(20)
  crypto.getRandomValues(bytes)
  let out = ''
  bytes.forEach((b, i) => {
    if (i > 0 && i % 5 === 0) out += '-'
    out += alphabet[b % alphabet.length]
  })
  return out
}
