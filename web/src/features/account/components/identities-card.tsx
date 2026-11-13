import { useAtomSet, useAtomValue } from '@effect/atom-react'
import { AsyncResultView } from '@/components/async-result-view'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { SsoButtons } from '@/features/auth/components/sso-buttons'
import type { AuthProvider } from '@/features/auth/schemas'
import { formatDate } from '@/lib/format'
import * as account from '../atoms'
import type { Identity } from '../schemas'

/** Linked single sign-on identities. Accounts are never linked by email; linking is explicit here. */
export function IdentitiesCard({ providers }: { providers: ReadonlyArray<AuthProvider> }) {
  const result = useAtomValue(account.identities)
  if (providers.length === 0) return null
  return (
    <Card>
      <CardHeader>
        <CardTitle>Single sign-on</CardTitle>
        <CardDescription>Sign in with an identity provider instead of a password.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <AsyncResultView result={result}>
          {(list) =>
            list.length === 0 ? (
              <p className="text-muted-foreground text-sm">No linked identities.</p>
            ) : (
              <ul className="divide-y rounded-lg border">{list.map((i) => <IdentityRow key={i.id} identity={i} />)}</ul>
            )
          }
        </AsyncResultView>
        <SsoButtons providers={providers} mode="link" label="Link" />
      </CardContent>
    </Card>
  )
}

function IdentityRow({ identity }: { identity: Identity }) {
  const unlink = useAtomSet(account.unlinkIdentity(identity.id))
  return (
    <li className="flex items-center justify-between gap-2 p-3 text-sm">
      <div>
        <div className="font-medium">{identity.label}</div>
        <div className="text-muted-foreground text-xs">{identity.email || 'no email'} · linked {formatDate(identity.created_at)}</div>
      </div>
      <Button variant="ghost" size="sm" onClick={() => unlink()}>Unlink</Button>
    </li>
  )
}
