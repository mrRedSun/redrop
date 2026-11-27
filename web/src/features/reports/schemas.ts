import { Schema } from 'effect'

/** Abuse report form (DESIGN.md "Abuse handling"). No account is needed. */

export const ReportCategory = Schema.Literals(['phishing', 'malware', 'illegal', 'spam', 'other'])
export type ReportCategory = typeof ReportCategory.Type

export const categoryLabels: Record<ReportCategory, string> = {
  phishing: 'Phishing or a fake sign-in page',
  malware: 'Malware or a harmful download',
  illegal: 'Illegal content',
  spam: 'Spam',
  other: 'Something else',
}

// ---- wire ----

export const ReportRequest = Schema.Struct({
  url: Schema.String,
  category: ReportCategory,
  details: Schema.optionalKey(Schema.String),
  contact: Schema.optionalKey(Schema.String),
})

// ---- form ----

/** Limits in characters (code points), as the server counts them. */
export const maxDetails = 2000
export const maxContact = 200

const atMost = (n: number, message: string) => Schema.makeFilter((s: string) => [...s].length <= n || message)

export const ReportForm = Schema.Struct({
  url: Schema.Trim.check(
    Schema.isNonEmpty({ message: 'Paste the address of the shared page' }),
    Schema.isMaxLength(2048, { message: 'At most 2048 characters' }),
  ),
  category: ReportCategory,
  details: Schema.Trim.check(atMost(maxDetails, `At most ${maxDetails} characters`)),
  contact: Schema.Trim.check(atMost(maxContact, `At most ${maxContact} characters`)),
})
export type ReportValues = typeof ReportForm.Type

/**
 * Links on redrop's own content pages carry the page in the fragment,
 * `#url=<page>`, which never reaches a server. Anything else starts empty.
 */
export const reportDefaults = (hash: string): typeof ReportForm.Encoded => ({
  url: new URLSearchParams(hash.replace(/^#/, '')).get('url') ?? '',
  category: 'phishing',
  details: '',
  contact: '',
})

export const toReportRequest = (v: ReportValues): typeof ReportRequest.Type => ({
  // Strip fragments even from invalid inputs so an e2e key never enters a request.
  url: v.url.split('#', 1)[0],
  category: v.category,
  ...(v.details !== '' && { details: v.details }),
  ...(v.contact !== '' && { contact: v.contact }),
})
