import { useAtomValue } from '@effect/atom-react'
import { AsyncResultView } from '@/components/async-result-view'
import { StatCard } from '@/components/stat-card'
import { formatBytes } from '@/lib/format'
import * as admin from '../atoms'

export function Component() {
  const result = useAtomValue(admin.stats)
  return (
    <AsyncResultView result={result}>
      {(s) => (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <StatCard label="Users" value={s.users} />
          <StatCard label="Artifacts" value={s.artifacts} hint={`${s.active_artifacts} active`} />
          <StatCard
            label="Storage (logical)"
            value={formatBytes(s.logical_bytes)}
            hint={s.max_total_bytes > 0 ? `of ${formatBytes(s.max_total_bytes)} server limit` : 'no server limit'}
            progress={s.max_total_bytes > 0 ? (s.logical_bytes / s.max_total_bytes) * 100 : undefined}
          />
          <StatCard label="On disk" value={formatBytes(s.stored_bytes)} hint={`deduplicated · ${formatBytes(s.max_version_bytes)} max per version`} />
        </div>
      )}
    </AsyncResultView>
  )
}
