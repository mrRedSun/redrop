import { useState } from 'react'
import { useAtomSet } from '@effect/atom-react'
import { Exit, Redacted } from 'effect'
import { Atom } from 'effect/reactivity'
import { ShieldCheck } from 'lucide-react'
import { useForm } from 'react-hook-form'
import QRCode from 'react-qr-code'
import { CopyField } from '@/components/copy-field'
import { FormRootError } from '@/components/form-root-error'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as account from '../atoms'
import { TotpCodeForm, totpCodeDefaults, type TotpSetup } from '../schemas'

/** Two-step verification: enroll with a QR code and confirm a code; disable with a code. */
export function TotpCard({ enabled }: { enabled: boolean }) {
  const [enrollment, setEnrollment] = useState<TotpSetup | null>(null)
  const begin = useAtomSet(account.beginTotp, { mode: 'promiseExit' })
  const resetBegin = useAtomSet(account.beginTotp)
  const finish = () => {
    setEnrollment(null)
    resetBegin(Atom.Reset) // drop the enrollment secret from the atom too
  }
  const start = async () => {
    const exit = await begin()
    if (Exit.isSuccess(exit)) setEnrollment(exit.value)
    resetBegin(Atom.Reset)
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          Two-step verification {enabled && <Badge><ShieldCheck className="size-3" /> on</Badge>}
        </CardTitle>
        <CardDescription>Password sign-ins also ask for a code from an authenticator app.</CardDescription>
      </CardHeader>
      <CardContent>
        {enabled ? (
          <CodeForm action={account.disableTotp} label="Turn off" description="Enter a current code to turn two-step verification off." />
        ) : enrollment ? (
          <div className="space-y-4">
            <div className="flex flex-wrap items-start gap-6">
              <div className="rounded-lg bg-white p-3"><QRCode value={Redacted.value(enrollment.otpauth_url)} size={160} /></div>
              <div className="min-w-64 flex-1 space-y-2">
                <p className="text-sm">Scan the code with your authenticator app, or enter this key manually:</p>
                <CopyField value={Redacted.value(enrollment.secret)} label="Setup key" />
              </div>
            </div>
            <CodeForm action={account.enableTotp} label="Turn on" description="Enter the code your app shows to finish." onDone={finish} />
            <Button variant="ghost" size="sm" onClick={finish}>Cancel</Button>
          </div>
        ) : (
          <Button variant="outline" onClick={start}>Set up authenticator</Button>
        )}
      </CardContent>
    </Card>
  )
}

type CodeAction = typeof account.enableTotp

function CodeForm({ action, label, description, onDone }: { action: CodeAction; label: string; description: string; onDone?: () => void }) {
  const form = useForm({ resolver: schemaResolver(TotpCodeForm), defaultValues: totpCodeDefaults })
  const onSubmit = useFormSubmit(form, action, { onSuccess: () => (form.reset(totpCodeDefaults), onDone?.()) })
  return (
    <Form {...form}>
      <form onSubmit={onSubmit} className="flex flex-wrap items-start gap-2">
        <FormField control={form.control} name="code" render={({ field }) => (
          <FormItem className="w-48">
            <FormLabel className="sr-only">Code</FormLabel>
            <FormControl><Input inputMode="numeric" autoComplete="one-time-code" placeholder="123456" {...field} /></FormControl>
            <FormMessage />
          </FormItem>
        )} />
        <Button type="submit" variant={label === 'Turn off' ? 'outline' : 'default'} disabled={form.formState.isSubmitting}>{label}</Button>
        <p className="text-muted-foreground basis-full text-xs">{description}</p>
        <div className="basis-full"><FormRootError control={form.control} /></div>
      </form>
    </Form>
  )
}
