import { useEffect, useRef, useState } from 'react'
import { useAtomValue } from '@effect/atom-react'
import { FileStack, Lock, Search } from 'lucide-react'
import { Link, useSearchParams } from 'react-router'
import { paths } from '@/app/paths'
import { AsyncResultView } from '@/components/async-result-view'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Input } from '@/components/ui/input'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useDebouncedValue } from '@/hooks/use-debounced-value'
import { formatBytes, relativeTime } from '@/lib/format'
import { useKeepPrevious } from '@/runtime/use-keep-previous'
import * as artifacts from '../atoms'
import { LinkActions } from '../components/link-actions'
import { StatusBadge } from '../components/status-badge'
import { defaultFilter, filterFromSearch, type Artifact } from '../schemas'

const statusTabs = ['all', 'active', 'expired', 'exhausted', 'revoked'] as const

/** Your artifacts. The URL owns the filter (shareable, back/forward works). */
export function Component() {
  const [search, setSearch] = useSearchParams()
  const filter = filterFromSearch(search)
  const result = useKeepPrevious(useAtomValue(artifacts.list(filter)))

  // Search box: local draft, debounced into the URL (replace, so typing does not spam history).
  const [draft, setDraft] = useState(filter.q)
  const q = useDebouncedValue(draft.trim(), 300)
  const writtenSearch = useRef(filter.q)
  useEffect(() => {
    // A back/forward navigation updates the input. A response to our own
    // debounced write must not overwrite text typed since that write.
    if (filter.q !== writtenSearch.current) {
      writtenSearch.current = filter.q
      setDraft(filter.q)
    }
  }, [filter.q])
  useEffect(() => {
    if (q === filter.q) return
    writtenSearch.current = q
    setSearch((p) => (q ? p.set('q', q) : p.delete('q'), p), { replace: true })
    // Only typing writes the URL; back/forward must not be undone by a stale draft.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q])

  const setStatus = (status: string) =>
    setSearch((p) => (status === defaultFilter.status ? p.delete('status') : p.set('status', status), p), { replace: true })

  return (
    <>
      <PageHeader title="Artifacts" description="Pages, files and sites you shared. Publish new ones with the redrop CLI." />
      <div className="flex flex-wrap items-center gap-3 pb-4">
        <Tabs value={filter.status} onValueChange={setStatus}>
          <TabsList>
            {statusTabs.map((s) => <TabsTrigger key={s} value={s} className="capitalize">{s}</TabsTrigger>)}
          </TabsList>
        </Tabs>
        <div className="relative ml-auto w-full sm:w-72">
          <Search className="text-muted-foreground absolute top-2.5 left-2.5 size-4" />
          <Input value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="Search title, id, or token" className="pl-8" />
        </div>
      </div>
      <AsyncResultView result={result} loading={<ArtifactTable rows={undefined} />}>
        {(items, waiting) => <ArtifactTable rows={[...items]} stale={waiting} filtered={filter.q !== '' || filter.status !== 'all'} />}
      </AsyncResultView>
    </>
  )
}

function ArtifactTable({ rows, stale, filtered }: { rows: Artifact[] | undefined; stale?: boolean; filtered?: boolean }) {
  return (
    <DataTable<Artifact>
      rows={rows}
      stale={stale}
      rowKey={(a) => a.id}
      empty={
        filtered
          ? <EmptyState icon={Search} title="Nothing matches" hint="Try another status or search term." />
          : <EmptyState icon={FileStack} title="No artifacts yet" hint={<>Create an API token, then run <code>redrop share ./report.html</code>.</>} />
      }
      columns={[
        {
          header: 'Title',
          cell: (a) => (
            <Link to={paths.artifact(a.id)} className="flex items-center gap-1.5 font-medium hover:underline">
              {a.has_password && <Lock className="text-muted-foreground size-3.5" aria-label="Password protected" />}
              <span className="max-w-72 truncate">{a.title || a.id}</span>
            </Link>
          ),
        },
        { header: 'Status', cell: (a) => <StatusBadge status={a.status} /> },
        { header: 'Kind', className: 'hidden md:table-cell', cell: (a) => <span className="text-muted-foreground">{a.kind}</span> },
        { header: 'Views', cell: (a) => (a.max_views ? `${a.views} / ${a.max_views}` : a.views) },
        { header: 'Expires', className: 'hidden md:table-cell', cell: (a) => (a.expires_at ? relativeTime(a.expires_at) : 'never') },
        { header: 'Size', className: 'hidden lg:table-cell text-right', cell: (a) => formatBytes(a.size) },
        { header: '', className: 'w-10', cell: (a) => <LinkActions artifact={a} /> },
      ]}
    />
  )
}
