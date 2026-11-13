import { useState } from 'react'
import { useAtomValue } from '@effect/atom-react'
import { MoreHorizontal } from 'lucide-react'
import { AsyncResultView } from '@/components/async-result-view'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Progress } from '@/components/ui/progress'
import { formatBytes, formatDate } from '@/lib/format'
import * as admin from '../atoms'
import { EditUserDialog } from '../components/edit-user-dialog'
import { ResetLinkDialog } from '../components/reset-link-dialog'
import type { User } from '../schemas'

export function Component() {
  const result = useAtomValue(admin.users)
  return (
    <AsyncResultView result={result}>
      {(users) => (
        <DataTable<User>
          rows={[...users]}
          rowKey={(u) => u.id}
          empty={<EmptyState title="No users" />}
          columns={[
            {
              header: 'User',
              cell: (u) => (
                <div>
                  <div className="font-medium">{u.display_name || u.username}</div>
                  <div className="text-muted-foreground text-xs">{u.username} · joined {formatDate(u.created_at)}</div>
                </div>
              ),
            },
            {
              header: 'Role',
              cell: (u) => (
                <div className="flex gap-1">
                  <Badge variant={u.role === 'admin' ? 'default' : 'secondary'}>{u.role}</Badge>
                  {u.disabled && <Badge variant="destructive">disabled</Badge>}
                  {u.totp_enabled && <Badge variant="outline">2FA</Badge>}
                </div>
              ),
            },
            {
              header: 'Storage',
              cell: (u) => (
                <div className="w-40 space-y-1">
                  <div className="text-xs">{formatBytes(u.used_bytes)} {u.quota_bytes > 0 ? `of ${formatBytes(u.quota_bytes)}` : '(no quota)'}</div>
                  {u.quota_bytes > 0 && <Progress aria-label={`${u.username} storage quota used`} value={Math.min(100, (u.used_bytes / u.quota_bytes) * 100)} />}
                </div>
              ),
            },
            { header: 'Artifacts', className: 'hidden md:table-cell', cell: (u) => u.artifacts },
            { header: '', className: 'w-10', cell: (u) => <UserActions user={u} /> },
          ]}
        />
      )}
    </AsyncResultView>
  )
}

function UserActions({ user }: { user: User }) {
  const [dialog, setDialog] = useState<'edit' | 'reset' | null>(null)
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" aria-label="User actions"><MoreHorizontal className="size-4" /></Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => setDialog('edit')}>Edit role, quota, status</DropdownMenuItem>
          <DropdownMenuItem onSelect={() => setDialog('reset')}>Create password reset link</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <EditUserDialog user={user} open={dialog === 'edit'} onOpenChange={(o) => setDialog(o ? 'edit' : null)} />
      {dialog === 'reset' && <ResetLinkDialog user={user} open onOpenChange={(o) => setDialog(o ? 'reset' : null)} />}
    </>
  )
}
