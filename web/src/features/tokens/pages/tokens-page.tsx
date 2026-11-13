import { useState } from 'react'
import { useAtomSet, useAtomValue } from '@effect/atom-react'
import { KeyRound, PartyPopper, Plus } from 'lucide-react'
import { useSearchParams } from 'react-router'
import { AsyncResultView } from '@/components/async-result-view'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { formatDate, relativeTime } from '@/lib/format'
import * as tokens from '../atoms'
import { CreateTokenDialog } from '../components/create-token-dialog'
import type { Token } from '../schemas'

/** API tokens. `?welcome=1` (after sign-up) explains the next step and opens the create dialog. */
export function Component() {
  const [params, setParams] = useSearchParams()
  const welcome = params.get('welcome') === '1'
  const [creating, setCreating] = useState(welcome)
  const result = useAtomValue(tokens.list)
  const dismissWelcome = () => setParams((p) => (p.delete('welcome'), p), { replace: true })

  return (
    <>
      <PageHeader
        title="API tokens"
        description="Tokens let agents and the CLI publish artifacts as you. They cannot manage tokens or your account."
        actions={<Button onClick={() => setCreating(true)}><Plus className="size-4" /> New token</Button>}
      />
      {welcome && (
        <Alert className="mb-6">
          <PartyPopper />
          <AlertTitle>Welcome to redrop</AlertTitle>
          <AlertDescription>
            Create a token and run the two commands shown to connect the CLI. <Button variant="link" className="h-auto p-0" onClick={dismissWelcome}>Dismiss</Button>
          </AlertDescription>
        </Alert>
      )}
      <AsyncResultView result={result}>
        {(list) => (
          <DataTable<Token>
            rows={[...list]}
            rowKey={(t) => t.id}
            empty={<EmptyState icon={KeyRound} title="No tokens yet" hint="Create one to connect the redrop CLI or an agent." />}
            columns={[
              { header: 'Name', cell: (t) => <span className="font-medium">{t.name}</span> },
              { header: 'Token', cell: (t) => <code className="text-xs">{t.prefix}…</code> },
              { header: 'Last used', cell: (t) => (t.last_used_at ? relativeTime(t.last_used_at) : 'never') },
              { header: 'Expires', className: 'hidden md:table-cell', cell: (t) => (t.expires_at ? formatDate(t.expires_at) : 'never') },
              { header: 'Artifacts', className: 'hidden md:table-cell', cell: (t) => t.artifacts },
              { header: '', className: 'text-right', cell: (t) => <RevokeButton token={t} /> },
            ]}
          />
        )}
      </AsyncResultView>
      {creating && <CreateTokenDialog open onOpenChange={setCreating} />}
    </>
  )
}

function RevokeButton({ token }: { token: Token }) {
  const [open, setOpen] = useState(false)
  const revoke = useAtomSet(tokens.revoke(token.id), { mode: 'promise' })
  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => setOpen(true)}>Revoke</Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title={`Revoke “${token.name}”?`}
        description="Anything using this token loses access immediately. Artifacts it created stay."
        confirmLabel="Revoke token"
        destructive
        onConfirm={() => revoke()}
      />
    </>
  )
}
