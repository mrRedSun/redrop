import { useState } from 'react'
import { useAtomRefresh, useAtomSet, useAtomSuspense, useAtomValue } from '@effect/atom-react'
import { Mail, Plus } from 'lucide-react'
import { AsyncResultView } from '@/components/async-result-view'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import * as auth from '@/features/auth/atoms'
import { formatBytes, formatDate, isPast, relativeTime } from '@/lib/format'
import * as admin from '../atoms'
import { CreateInviteDialog } from '../components/create-invite-dialog'
import type { Invite } from '../schemas'

export function Component() {
  const result = useAtomValue(admin.invites)
  const refresh = useAtomRefresh(admin.invites)
  const config = useAtomSuspense(auth.config).value
  const [creating, setCreating] = useState(false)
  return (
    <>
      <div className="flex justify-end gap-2 pb-4">
        {config.mail && <Button variant="outline" disabled={result.waiting} onClick={refresh}>Refresh delivery status</Button>}
        <Button onClick={() => setCreating(true)}><Plus className="size-4" /> New invite</Button>
      </div>
      <AsyncResultView result={result}>
        {(invites, waiting) => (
          <DataTable<Invite>
            rows={[...invites]}
            stale={waiting}
            rowKey={(i) => i.id}
            empty={<EmptyState icon={Mail} title="No invites" hint="redrop is invite-only. Create a link for each new user." />}
            columns={[
              { header: 'Note', cell: (i) => i.note || <span className="text-muted-foreground">—</span> },
              { header: 'Role', cell: (i) => <Badge variant={i.role === 'admin' ? 'default' : 'secondary'}>{i.role}</Badge> },
              { header: 'Quota', className: 'hidden md:table-cell', cell: (i) => formatBytes(i.quota_bytes) },
              { header: 'Status', cell: (i) => <InviteStatus invite={i} /> },
              { header: 'Email', cell: (i) => i.email ? <span className="break-all">{i.email}</span> : <span className="text-muted-foreground">No email</span> },
              { header: 'Email status', cell: (i) => <InviteEmailStatus invite={i} /> },
              { header: 'Created', className: 'hidden md:table-cell', cell: (i) => formatDate(i.created_at) },
              { header: '', className: 'text-right', cell: (i) => (
                <div className="flex justify-end gap-1">
                  {config.mail && i.email && !i.used_at && !isPast(i.expires_at) && <ResendInvite invite={i} disabled={waiting} />}
                  {!i.used_at && <RevokeInvite id={i.id} />}
                </div>
              ) },
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

function InviteEmailStatus({ invite: i }: { invite: Invite }) {
  if (!i.email_status) return <span className="text-muted-foreground">—</span>
  return (
    <div className="space-y-1">
      <Badge variant={i.email_status === 'failed' ? 'destructive' : 'secondary'}>{i.email_status}</Badge>
      {i.email_at && <p className="text-muted-foreground text-xs">{formatDate(i.email_at)}</p>}
      {i.email_status === 'failed' && i.email_error && <p className="max-w-xs whitespace-normal break-words text-xs">{i.email_error}</p>}
    </div>
  )
}

function ResendInvite({ invite, disabled }: { invite: Invite; disabled: boolean }) {
  const atom = admin.resendInvite(invite.id)
  const resend = useAtomSet(atom)
  const result = useAtomValue(atom)
  return <Button variant="ghost" size="sm" disabled={disabled || result.waiting || invite.email_status === 'queued' || invite.email_status === 'sending'} onClick={() => { if (!isPast(invite.expires_at)) resend() }}>Send again</Button>
}

function RevokeInvite({ id }: { id: string }) {
  const revoke = useAtomSet(admin.deleteInvite(id))
  return <Button variant="ghost" size="sm" onClick={() => revoke()}>Revoke</Button>
}
