import { useAtomSuspense } from '@effect/atom-react'
import { Option } from 'effect'
import { AlertCircle } from 'lucide-react'
import { useSearchParams } from 'react-router'
import { PageHeader } from '@/components/page-header'
import { Alert, AlertDescription } from '@/components/ui/alert'
import * as auth from '@/features/auth/atoms'
import { IdentitiesCard } from '../components/identities-card'
import { PasswordCard } from '../components/password-card'
import { ProfileCard } from '../components/profile-card'
import { TotpCard } from '../components/totp-card'

/** Profile, password, two-step verification, and SSO identities. Handles the SSO link round trip (?linked=1, ?error=). */
export function Component() {
  const me = Option.getOrThrow(useAtomSuspense(auth.session).value)
  const config = useAtomSuspense(auth.config).value
  const [params] = useSearchParams()
  const linked = params.get('linked') === '1'
  const error = params.get('error')


  return (
    <>
      <PageHeader title="Account" />
      {linked && <Alert className="mb-6"><AlertDescription>Identity linked.</AlertDescription></Alert>}
      {error && (
        <Alert variant="destructive" className="mb-6">
          <AlertCircle />
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="grid gap-6">
        <ProfileCard me={me} />
        {config.password_login && <PasswordCard />}
        {config.password_login && <TotpCard enabled={me.user.totp_enabled} />}
        <IdentitiesCard providers={config.providers ?? []} />
      </div>
    </>
  )
}
