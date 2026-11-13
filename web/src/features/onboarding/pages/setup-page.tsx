import { useAtomSuspense } from '@effect/atom-react'
import { useParams } from 'react-router'
import { paths } from '@/app/paths'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import * as auth from '@/features/auth/atoms'
import * as onboarding from '../atoms'
import { LinkInvalid } from '../components/link-invalid'
import { NewAccountFields } from '../components/new-account-form'

/** First run: the one-time link from the server log creates the first admin. */
export function Component() {
  const token = useParams().token ?? ''
  const config = useAtomSuspense(auth.config).value
  if (!config.setup_required) return <LinkInvalid what="setup" />
  return (
    <Card>
      <CardHeader>
        <CardTitle>Create the admin account</CardTitle>
        <CardDescription>This server has no users yet. The account you create here is its first administrator.</CardDescription>
      </CardHeader>
      <CardContent>
        <NewAccountFields submit={onboarding.setup(token)} label="Create admin account" onDone={() => window.location.replace(paths.tokensWelcome)} />
      </CardContent>
    </Card>
  )
}
