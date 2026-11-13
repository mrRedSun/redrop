import { useState } from 'react'
import { useAtomValue } from '@effect/atom-react'
import { AsyncResultView } from '@/components/async-result-view'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { formatBytes } from '@/lib/format'
import * as artifacts from '../atoms'
import type { FileEntry, Version } from '../schemas'

/** Files of the live version or any retained version. */
export function FilesPanel({ id, versions }: { id: string; versions: ReadonlyArray<Version> }) {
  const [version, setVersion] = useState(0)
  const result = useAtomValue(artifacts.files(artifacts.filesKey(id, version)))
  return (
    <div className="space-y-4">
      {versions.length > 1 && (
        <Select value={String(version)} onValueChange={(v) => setVersion(Number(v))}>
          <SelectTrigger className="w-56"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value="0">Live version</SelectItem>
            {versions.map((v) => <SelectItem key={v.number} value={String(v.number)}>Version {v.number}</SelectItem>)}
          </SelectContent>
        </Select>
      )}
      <AsyncResultView result={result}>
        {(list, waiting) => (
          <DataTable<FileEntry>
            rows={[...list.items]}
            stale={waiting}
            rowKey={(f) => f.path}
            empty={<EmptyState title="No files" />}
            columns={[
              { header: 'Path', cell: (f) => <span className="font-mono text-xs break-all">{f.path}</span> },
              { header: 'Type', cell: (f) => <span className="text-muted-foreground text-xs">{f.content_type.split(';')[0]}</span> },
              { header: 'Size', className: 'text-right', cell: (f) => formatBytes(f.size) },
            ]}
          />
        )}
      </AsyncResultView>
    </div>
  )
}
