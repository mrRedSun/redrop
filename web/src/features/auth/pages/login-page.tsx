import { useState } from 'react'
import { useAtomSuspense } from '@effect/atom-react'
import { AlertCircle } from 'lucide-react'
import { useForm } from 'react-hook-form'
import { useSearchParams } from 'react-router'
import { safeNext } from '@/app/paths'
import { FormRootError } from '@/components/form-root-error'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Separator } from '@/components/ui/separator'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as auth from '../atoms'
import { SsoButtons } from '../components/sso-buttons'
import { CredentialsForm, TotpForm, credentialsDefaults, totpDefaults, type Credentials, type LoginOutcome } from '../schemas'

/** Sign in: password (then the authenticator code when enabled) or single sign-on. */
export function Component() {
  const [params] = useSearchParams()
  const config = useAtomSuspense(auth.config).value
  // Step 2 holds the already-validated credentials; nothing else survives the page.
  const [pending, setPending] = useState<Credentials | null>(null)

  // `login` invalidated the session; the target route's guard waits for the fresh `me`.
  const onOutcome = (outcome: LoginOutcome, credentials: Credentials) =>
    outcome._tag === 'SignedIn' ? window.location.replace(safeNext(params.get('next'))) : setPending(credentials)

  const providers = config.providers ?? []
  const ssoError = params.get('error')
  return (
    <Card>
      <CardHeader>
        <CardTitle>{pending ? 'Two-step verification' : 'Sign in'}</CardTitle>
        <CardDescription>{pending ? 'Enter the code from your authenticator app.' : 'Manage your shared artifacts.'}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {ssoError && (
          <Alert variant="destructive">
            <AlertCircle />
            <AlertDescription>{ssoError}</AlertDescription>
          </Alert>
        )}
        {pending ? (
          <TotpStep credentials={pending} onSignedIn={(o) => onOutcome(o, pending)} onBack={() => setPending(null)} />
        ) : (
          <>
            {config.password_login && <PasswordStep onOutcome={onOutcome} />}
            {config.password_login && providers.length > 0 && <Separator />}
            <SsoButtons providers={providers} mode="login" />
          </>
        )}
      </CardContent>
    </Card>
  )
}

function PasswordStep({ onOutcome }: { onOutcome: (o: LoginOutcome, c: Credentials) => void }) {
  const form = useForm({ resolver: schemaResolver(CredentialsForm), defaultValues: credentialsDefaults })
  const onSubmit = useFormSubmit(form, auth.login, { toArg: (credentials) => ({ credentials }), onSuccess: onOutcome })
  return (
    <Form {...form}>
      <form onSubmit={onSubmit} className="space-y-4">
        <FormField control={form.control} name="username" render={({ field }) => (
          <FormItem>
            <FormLabel>Username</FormLabel>
            <FormControl><Input autoComplete="username" autoFocus {...field} /></FormControl>
            <FormMessage />
          </FormItem>
        )} />
        <FormField control={form.control} name="password" render={({ field }) => (
          <FormItem>
            <FormLabel>Password</FormLabel>
            <FormControl><Input type="password" autoComplete="current-password" {...field} /></FormControl>
            <FormMessage />
          </FormItem>
        )} />
        <FormRootError control={form.control} />
        <Button type="submit" className="w-full" disabled={form.formState.isSubmitting}>Sign in</Button>
      </form>
    </Form>
  )
}

function TotpStep(props: { credentials: Credentials; onSignedIn: (o: LoginOutcome) => void; onBack: () => void }) {
  const form = useForm({ resolver: schemaResolver(TotpForm), defaultValues: totpDefaults })
  const onSubmit = useFormSubmit(form, auth.login, {
    toArg: ({ code }) => ({ credentials: props.credentials, totp: code }),
    onSuccess: props.onSignedIn,
  })
  return (
    <Form {...form}>
      <form onSubmit={onSubmit} className="space-y-4">
        <FormField control={form.control} name="code" render={({ field }) => (
          <FormItem>
            <FormLabel>Authenticator code</FormLabel>
            <FormControl><Input inputMode="numeric" autoComplete="one-time-code" autoFocus {...field} /></FormControl>
            <FormMessage />
          </FormItem>
        )} />
        <FormRootError control={form.control} />
        <Button type="submit" className="w-full" disabled={form.formState.isSubmitting}>Verify</Button>
        <Button type="button" variant="link" className="w-full" onClick={props.onBack}>Use a different account</Button>
      </form>
    </Form>
  )
}
