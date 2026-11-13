import { useState } from 'react'
import { useAtomSet } from '@effect/atom-react'
import { Copy, ExternalLink, MoreHorizontal, RefreshCw, RotateCcw, ShieldOff, Trash2 } from 'lucide-react'
import { useNavigate } from 'react-router'
import { paths } from '@/app/paths'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { useCopy } from '@/hooks/use-copy'
import * as artifacts from '../atoms'
import type { Artifact } from '../schemas'

/**
 * Owner actions on one artifact: copy/open link, revoke/restore, rotate, delete.
 * Per-id atoms give each row its own pending state; toasts and refreshes are
 * declared on the atoms.
 */
export function LinkActions({ artifact, afterDelete }: { artifact: Artifact; afterDelete?: 'list' }) {
  const copy = useCopy()
  const navigate = useNavigate()
  const setRevoked = useAtomSet(artifacts.setRevoked(artifact.id))
  const rotate = useAtomSet(artifacts.rotate(artifact.id), { mode: 'promise' })
  const remove = useAtomSet(artifacts.remove(artifact.id), { mode: 'promise' })
  const [confirm, setConfirm] = useState<'rotate' | 'delete' | null>(null)
  const revoked = artifact.status === 'revoked'
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" aria-label="Artifact actions">
            <MoreHorizontal className="size-4" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onSelect={() => copy(artifact.url, 'Link copied')}>
            <Copy /> Copy link
          </DropdownMenuItem>
          <DropdownMenuItem asChild>
            <a href={artifact.url} target="_blank" rel="noreferrer noopener">
              <ExternalLink /> Open link
            </a>
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={() => setRevoked(!revoked)}>
            {revoked ? <RotateCcw /> : <ShieldOff />} {revoked ? 'Restore' : 'Revoke'}
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => setConfirm('rotate')}>
            <RefreshCw /> Replace link
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem variant="destructive" onSelect={() => setConfirm('delete')}>
            <Trash2 /> Delete
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <ConfirmDialog
        open={confirm === 'rotate'}
        onOpenChange={(o) => !o && setConfirm(null)}
        title="Replace the link?"
        description="A new secret link is created. Everyone using the old link loses access."
        confirmLabel="Replace link"
        onConfirm={() => rotate()}
      />
      <ConfirmDialog
        open={confirm === 'delete'}
        onOpenChange={(o) => !o && setConfirm(null)}
        title="Delete this artifact?"
        description={<>“{artifact.title || artifact.id}” and all its versions are removed. The link stops working immediately.</>}
        confirmLabel="Delete"
        destructive
        onConfirm={async () => {
          await remove()
          if (afterDelete === 'list') navigate(paths.artifacts, { replace: true })
        }}
      />
    </>
  )
}
