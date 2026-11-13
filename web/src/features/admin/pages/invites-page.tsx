import { useState } from 'react'
import { useAtomSet, useAtomValue } from '@effect/atom-react'
import { Mail, Plus } from 'lucide-react'
import { AsyncResultView } from '@/components/async-result-view'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { formatBytes, formatDate, isPast, relativeTime } from '@/lib/format'
import * as admin from '../atoms'
import { CreateInviteDialog } from '../components/create-invite-dialog'
import type { Invite } from '../schemas'

export function Component() {
  const result = useAtomValue(admin.invites)
  const [creating, setCreating] = useState(false)
  return (
    <>
      <div className="flex justify-end pb-4">
        <Button onClick={() => setCreating(true)}><Plus className="size-4" /> New invite</Button>
      </div>
      <AsyncResultView result={result}>
        {(invites) => (
          <DataTable<Invite>
            rows={[...invites]}
            rowKey={(i) => i.id}
            empty={<EmptyState icon={Mail} title="No invites" hint="redrop is invite-only. Create a link for each new user." />}
            columns={[
              { header: 'Note', cell: (i) => i.note || <span className="text-muted-foreground">—</span> },
              { header: 'Role', cell: (i) => <Badge variant={i.role === 'admin' ? 'default' : 'secondary'}>{i.role}</Badge> },
              { header: 'Quota', className: 'hidden md:table-cell', cell: (i) => formatBytes(i.quota_bytes) },
              { header: 'Status', cell: (i) => <InviteStatus invite={i} /> },
              { header: 'Created', className: 'hidden md:table-cell', cell: (i) => formatDate(i.created_at) },
              { header: '', className: 'text-right', cell: (i) => (i.used_at ? null : <RevokeInvite id={i.id} />) },
            ]}
          />
        )}
      </AsyncResultView>
      {creating && <CreateInviteDialog open onOpenChange={setCreating} />}
    </>
  )
}

function InviteStatus({ invite: i }: { invite: Invite }) {
  if (i.used_at) return <span className="text-sm">used by {i.used_by ?? 'someone'} {relativeTime(i.used_at)}</span>
  if (isPast(i.expires_at)) return <Badge variant="outline">expired</Badge>
  return <span className="text-sm">open · expires {relativeTime(i.expires_at)}</span>
}

function RevokeInvite({ id }: { id: string }) {
  const revoke = useAtomSet(admin.deleteInvite(id))
  return <Button variant="ghost" size="sm" onClick={() => revoke()}>Revoke</Button>
}
