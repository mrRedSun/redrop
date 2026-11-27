import { useState } from 'react'
import { useAtomSet, useAtomValue } from '@effect/atom-react'
import { ExternalLink, Flag, ShieldAlert } from 'lucide-react'
import { useSearchParams } from 'react-router'
import { AsyncResultView } from '@/components/async-result-view'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { EmptyState } from '@/components/empty-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { formatDate, relativeTime } from '@/lib/format'
import { useKeepPrevious } from '@/runtime/use-keep-previous'
import { statusLabel } from '@/features/artifacts/schemas'
import * as admin from '../atoms'
import { reportFilterFromSearch, reportSearch, reportedLink, type Report, type ReportFilter } from '../schemas'

const filters = ['open', 'resolved', 'dismissed', 'all'] as const

/**
 * The abuse queue (DESIGN.md "Abuse handling"). Nothing here acts on its
 * own: an administrator reads each report and decides. Reporter text and
 * upload findings are untrusted and rendered as plain text.
 */
export function Component() {
  const [search, setSearch] = useSearchParams()
  const filter = reportFilterFromSearch(search)
  const result = useKeepPrevious(useAtomValue(admin.reports(admin.reportsKey(filter))))
  const setFilter = (patch: Partial<ReportFilter>) => setSearch((p) => reportSearch(p, patch), { replace: true })
  return (
    <>
      <div className="mb-4 flex flex-wrap gap-3">
        <Tabs value={filter.status} onValueChange={(status) => setFilter({ status: status as ReportFilter['status'] })}>
          <TabsList aria-label="Report status">
            {filters.map((f) => <TabsTrigger key={f} value={f} className="capitalize">{f}</TabsTrigger>)}
          </TabsList>
        </Tabs>
        <Tabs value={filter.source} onValueChange={(source) => setFilter({ source: source as ReportFilter['source'] })}>
          <TabsList aria-label="Report source">
            <TabsTrigger value="all">All sources</TabsTrigger>
            <TabsTrigger value="visitor">Visitors</TabsTrigger>
            <TabsTrigger value="system">Upload checks</TabsTrigger>
          </TabsList>
        </Tabs>
      </div>
      <AsyncResultView result={result}>
        {(page, waiting) => (
          <div className="space-y-4" aria-busy={waiting}>
            <div className="flex flex-wrap items-center justify-between gap-3">
              <p className="text-muted-foreground text-sm" role="status">
                {waiting ? 'Loading reports...' : page.items.length === 0 ? `0 shown of ${page.total} reports` : `Showing ${(page.page - 1) * page.page_size + 1} to ${(page.page - 1) * page.page_size + page.items.length} of ${page.total} reports`}
              </p>
              <nav aria-label="Report pages" className="flex items-center gap-2">
                <Button variant="outline" size="sm" disabled={waiting || page.page <= 1} onClick={() => setFilter({ page: page.page - 1 })}>Previous</Button>
                <span className="text-muted-foreground text-sm">Page {page.page} of {Math.max(1, Math.ceil(page.total / page.page_size))}</span>
                <Button variant="outline" size="sm" disabled={waiting || page.page * page.page_size >= page.total} onClick={() => setFilter({ page: page.page + 1 })}>Next</Button>
              </nav>
            </div>
            {page.items.length === 0 ? (
              <EmptyState icon={Flag} title={filter.status === 'open' ? 'No open reports on this page' : 'No reports on this page'} hint="Visitors report pages from the link on gate pages and in the info panel. Uploads with a password form posting elsewhere, or with executables, are flagged here too." />
            ) : page.items.map((r) => <ReportCard key={r.id} report={r} waiting={waiting} />)}
          </div>
        )}
      </AsyncResultView>
    </>
  )
}

function ReportCard({ report: r, waiting }: { report: Report; waiting: boolean }) {
  const link = reportedLink(r)
  const a = r.artifact
  return (
    <Card>
      <CardHeader className="flex flex-row flex-wrap items-center gap-2">
        <Badge variant={r.category === 'phishing' || r.category === 'malware' ? 'destructive' : 'secondary'} className="capitalize">{r.category}</Badge>
        <Badge variant="outline">{r.source === 'system' ? `Upload check, version ${r.version}` : 'Visitor report'}</Badge>
        <Badge variant={r.status === 'open' ? 'default' : 'secondary'} className="capitalize">{r.status}</Badge>
        <span className="text-muted-foreground ml-auto text-sm" title={formatDate(r.created_at)}>{relativeTime(r.created_at)}</span>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <dl className="grid gap-x-4 gap-y-1 sm:grid-cols-[8rem_1fr]">
          <dt className="text-muted-foreground">Artifact</dt>
          <dd className="min-w-0 break-words">
            {a ? (
              <>
                <span className="font-medium">{a.title || a.id}</span> <span className="text-muted-foreground">({a.kind}, {statusLabel(a)})</span>
              </>
            ) : (
              <span className="text-muted-foreground">deleted ({r.artifact_id})</span>
            )}
          </dd>
          <dt className="text-muted-foreground">Owner</dt>
          <dd>
            {r.owner_username || 'unknown'}
            {r.owner_suspended && <Badge variant="destructive" className="ml-2">suspended</Badge>}
          </dd>
          {r.details && (
            <>
              <dt className="text-muted-foreground">{r.source === 'system' ? 'Finding' : 'Details'}</dt>
              <dd className="min-w-0 whitespace-pre-wrap break-words">{r.details}</dd>
            </>
          )}
          {r.contact && (
            <>
              <dt className="text-muted-foreground">Contact</dt>
              <dd className="min-w-0 break-words">{r.contact} <span className="text-muted-foreground">(unverified)</span></dd>
            </>
          )}
          {link && (
            <>
              <dt className="text-muted-foreground">Reported link</dt>
              <dd className="min-w-0">
                <a href={link} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 break-all underline underline-offset-4">
                  Open reported link<ExternalLink className="size-3.5 shrink-0" aria-hidden />
                </a>
                <p className="text-muted-foreground text-xs">Opening it is an ordinary visit: it counts a view, a password still applies, and the page may be hostile.</p>
              </dd>
            </>
          )}
        </dl>
        {(r.history ?? []).length > 0 && (
          <ol className="text-muted-foreground border-l pl-3 text-xs">
            {(r.history ?? []).map((e, i) => (
              <li key={i}>
                <span title={formatDate(e.at)}>{relativeTime(e.at)}</span>: {e.actor}, {e.action}{e.detail && ` (${e.detail})`}
              </li>
            ))}
          </ol>
        )}
        {!waiting && <ReportActions report={r} />}
      </CardContent>
    </Card>
  )
}

function ReportActions({ report: r }: { report: Report }) {
  const setStatus = useAtomSet(admin.setReportStatus(r.id))
  const revoke = useAtomSet(admin.revokeReported(r.id), { mode: 'promise' })
  const suspend = useAtomSet(admin.suspendReportedOwner(r.id), { mode: 'promise' })
  const [confirm, setConfirm] = useState<'revoke' | 'suspend' | null>(null)
  const title = r.artifact ? `“${r.artifact.title || r.artifact.id}”` : 'the artifact'
  return (
    <div className="flex flex-wrap gap-2 pt-1">
      {r.status === 'open' ? (
        <>
          <Button size="sm" variant="outline" onClick={() => setStatus('resolved')}>Resolve</Button>
          <Button size="sm" variant="outline" onClick={() => setStatus('dismissed')}>Dismiss</Button>
        </>
      ) : (
        <Button size="sm" variant="outline" onClick={() => setStatus('open')}>Reopen</Button>
      )}
      {r.artifact && !r.artifact.revoked_by_admin && (
        <Button size="sm" variant="destructive" onClick={() => setConfirm('revoke')}><ShieldAlert className="size-4" aria-hidden />Revoke artifact</Button>
      )}
      {r.owner_username && !r.owner_suspended && (
        <Button size="sm" variant="destructive" onClick={() => setConfirm('suspend')}>Suspend owner</Button>
      )}
      <ConfirmDialog
        open={confirm === 'revoke'}
        onOpenChange={(o) => !o && setConfirm(null)}
        title="Revoke this artifact?"
        description={<>{title} stops working at once. Its owner can't restore it; you can, from the Artifacts tab. Its open reports are resolved.</>}
        confirmLabel="Revoke"
        destructive
        onConfirm={() => revoke()}
      />
      <ConfirmDialog
        open={confirm === 'suspend'}
        onOpenChange={(o) => !o && setConfirm(null)}
        title={`Suspend ${r.owner_username}?`}
        description={<>Their sessions end, they can't sign in, their API tokens stop working, and every artifact they own is revoked. Re-enabling the account on the Users tab does not restore the artifacts; restore each one from the Artifacts tab.</>}
        confirmLabel="Suspend"
        destructive
        onConfirm={() => suspend()}
      />
    </div>
  )
}
