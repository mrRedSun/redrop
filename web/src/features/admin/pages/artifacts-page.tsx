import { useState } from 'react'
import { useAtomSet, useAtomValue } from '@effect/atom-react'
import { Lock, MoreHorizontal, Search } from 'lucide-react'
import { AsyncResultView } from '@/components/async-result-view'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { useDebouncedValue } from '@/hooks/use-debounced-value'
import { formatBytes, relativeTime } from '@/lib/format'
import { useKeepPrevious } from '@/runtime/use-keep-previous'
import * as admin from '../atoms'
import type { AdminArtifact } from '../schemas'

/** Every user's artifacts, metadata only: no links, no previews. Revoke or delete for abuse. */
export function Component() {
  const [draft, setDraft] = useState('')
  const q = useDebouncedValue(draft.trim(), 300)
  const result = useKeepPrevious(useAtomValue(admin.artifacts(admin.artifactsKey('all', q))))
  return (
    <>
      <div className="relative mb-4 w-full sm:w-72">
        <Search className="text-muted-foreground absolute top-2.5 left-2.5 size-4" />
        <Input value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="Search title, id, or token" className="pl-8" />
      </div>
      <AsyncResultView result={result}>
        {(items, waiting) => (
          <DataTable<AdminArtifact>
            rows={[...items]}
            stale={waiting}
            rowKey={(a) => a.id}
            empty={<EmptyState icon={Search} title="No artifacts" />}
            columns={[
              {
                header: 'Title',
                cell: (a) => (
                  <span className="flex items-center gap-1.5">
                    {a.has_password && <Lock className="text-muted-foreground size-3.5" aria-label="Password protected" />}
                    <span className="max-w-64 truncate">{a.title || a.id}</span>
                  </span>
                ),
              },
              { header: 'Owner', cell: (a) => a.owner_username },
              { header: 'Status', cell: (a) => <Badge variant={a.status === 'revoked' ? 'destructive' : a.status === 'active' ? 'default' : 'secondary'}>{a.status}</Badge> },
              { header: 'Views', className: 'hidden md:table-cell', cell: (a) => (a.max_views ? `${a.views} / ${a.max_views}` : a.views) },
              { header: 'Size', className: 'hidden md:table-cell', cell: (a) => formatBytes(a.total_size) },
              { header: 'Expires', className: 'hidden lg:table-cell', cell: (a) => (a.expires_at ? relativeTime(a.expires_at) : 'never') },
              { header: '', className: 'w-10', cell: (a) => <ModerationActions artifact={a} /> },
            ]}
          />
        )}
      </AsyncResultView>
    </>
  )
}

function ModerationActions({ artifact }: { artifact: AdminArtifact }) {
  const setRevoked = useAtomSet(admin.setArtifactRevoked(artifact.id))
  const remove = useAtomSet(admin.deleteArtifact(artifact.id), { mode: 'promise' })
  const [confirming, setConfirming] = useState(false)
  const revoked = artifact.status === 'revoked'
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" aria-label="Moderation actions"><MoreHorizontal className="size-4" /></Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => setRevoked(!revoked)}>{revoked ? 'Restore' : 'Revoke'}</DropdownMenuItem>
          <DropdownMenuItem variant="destructive" onSelect={() => setConfirming(true)}>Delete</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title="Delete this artifact?"
        description={<>“{artifact.title || artifact.id}” owned by {artifact.owner_username} is removed with all versions. The owner is not notified.</>}
        confirmLabel="Delete"
        destructive
        onConfirm={() => remove()}
      />
    </>
  )
}
