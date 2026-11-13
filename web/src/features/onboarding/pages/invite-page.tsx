import { useAtomSuspense } from '@effect/atom-react'
import { useParams } from 'react-router'
import { paths } from '@/app/paths'
import { Separator } from '@/components/ui/separator'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { SsoButtons } from '@/features/auth/components/sso-buttons'
import { formatDate } from '@/lib/format'
import * as onboarding from '../atoms'
import { LinkInvalid } from '../components/link-invalid'
import { NewAccountFields } from '../components/new-account-form'

/** Invite acceptance: create a password account, or sign up through single sign-on. */
export function Component() {
  const token = useParams().token ?? ''
  const info = useAtomSuspense(onboarding.inviteInfo(token)).value
  if (!info.valid) return <LinkInvalid what="invite" />
  const providers = info.providers ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle>You're invited to redrop</CardTitle>
        <CardDescription>
          Create your {info.role === 'admin' ? 'administrator ' : ''}account. This invite expires {formatDate(info.expires_at)}.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <NewAccountFields submit={onboarding.acceptInvite(token)} label="Create account" onDone={() => window.location.replace(paths.tokensWelcome)} />
        {providers.length > 0 && <Separator />}
        <SsoButtons providers={providers} mode="invite" inviteToken={token} label="Sign up with" />
      </CardContent>
    </Card>
  )
}
