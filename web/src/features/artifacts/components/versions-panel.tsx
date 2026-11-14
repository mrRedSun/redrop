import { useState } from 'react'
import { useAtomSet, useAtomValue } from '@effect/atom-react'
import { AsyncResultView } from '@/components/async-result-view'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { formatBytes, formatDate } from '@/lib/format'
import * as artifacts from '../atoms'
import type { Version } from '../schemas'

/** Version history: make an older version current (rollback) or delete it. The link stays the same. */
export function VersionsPanel({ id }: { id: string }) {
  const result = useAtomValue(artifacts.versions(id))
  const activate = useAtomSet(artifacts.activateVersion(id))
  const remove = useAtomSet(artifacts.deleteVersion(id), { mode: 'promise' })
  const [deleting, setDeleting] = useState<number | null>(null)
  return (
    <AsyncResultView result={result}>
      {(versions) => (
        <>
          <p className="text-muted-foreground mb-3 text-sm">Notes on new versions are shown to viewers in the info panel's version list. Notes written before that stay private.</p>
          <DataTable<Version>
            rows={[...versions]}
            rowKey={(v) => String(v.number)}
            empty={<EmptyState title="No versions" />}
            columns={[
              { header: 'Version', cell: (v) => <span className="flex items-center gap-2">v{v.number}{v.current && <Badge>live</Badge>}</span> },
              { header: 'Published', cell: (v) => formatDate(v.created_at) },
              { header: 'Size', cell: (v) => `${formatBytes(v.size)} · ${v.files} file(s)` },
              { header: 'Note', cell: (v) => <span className="text-muted-foreground">{v.note || (v.token_name ? `via ${v.token_name}` : '—')}{v.note && <Badge variant="outline" className="ml-2">{v.note_public ? 'shown to viewers' : 'private'}</Badge>}</span> },
              {
                header: '',
                className: 'text-right',
                cell: (v) =>
                  v.current ? null : (
                    <div className="flex justify-end gap-2">
                      <Button size="sm" variant="outline" onClick={() => activate(v.number)}>Make live</Button>
                      <Button size="sm" variant="ghost" onClick={() => setDeleting(v.number)}>Delete</Button>
                    </div>
                  ),
              },
            ]}
          />
          <ConfirmDialog
            open={deleting !== null}
            onOpenChange={(o) => !o && setDeleting(null)}
            title={`Delete version ${deleting}?`}
            description="This version's files are removed. The live version is not affected."
            confirmLabel="Delete version"
            destructive
            onConfirm={() => (deleting === null ? undefined : remove(deleting))}
          />
        </>
      )}
    </AsyncResultView>
  )
}
