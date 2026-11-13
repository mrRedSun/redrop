import { useAtomValue } from '@effect/atom-react'
import { Activity } from 'lucide-react'
import { AsyncResultView } from '@/components/async-result-view'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { Badge } from '@/components/ui/badge'
import { formatDate } from '@/lib/format'
import * as artifacts from '../atoms'
import type { View } from '../schemas'

const outcomes: Record<string, { label: string; variant: 'default' | 'secondary' | 'destructive' | 'outline' }> = {
  view: { label: 'view', variant: 'default' },
  preview: { label: 'link preview', variant: 'secondary' },
  denied: { label: 'denied', variant: 'outline' },
  bad_password: { label: 'wrong password', variant: 'destructive' },
}

/** Recent access log (newest first, up to 200 entries). */
export function AccessLogPanel({ id }: { id: string }) {
  const result = useAtomValue(artifacts.views(id))
  return (
    <AsyncResultView result={result}>
      {(views) => (
        <DataTable<View>
          rows={[...views]}
          rowKey={(v) => String(views.indexOf(v))}
          empty={<EmptyState icon={Activity} title="No visits yet" hint="Views, link previews and wrong passwords appear here." />}
          columns={[
            { header: 'When', cell: (v) => formatDate(v.at) },
            { header: 'Outcome', cell: (v) => { const o = outcomes[v.outcome] ?? { label: v.outcome, variant: 'outline' as const }; return <Badge variant={o.variant}>{o.label}</Badge> } },
            { header: 'Client', cell: (v) => v.client },
            { header: 'Country', cell: (v) => v.country || '—' },
          ]}
        />
      )}
    </AsyncResultView>
  )
}
