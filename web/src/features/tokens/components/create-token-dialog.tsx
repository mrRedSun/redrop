import { useAtomSet, useAtomSuspense } from '@effect/atom-react'
import { Redacted } from 'effect'
import { Atom } from 'effect/reactivity'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { CopyField } from '@/components/copy-field'
import { FormRootError } from '@/components/form-root-error'
import { SecretReveal } from '@/components/secret-reveal'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import * as auth from '@/features/auth/atoms'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as tokens from '../atoms'
import { CreateTokenForm, createTokenDefaults, type CreatedToken } from '../schemas'
import { setupCommand, setupOsFromPlatform, type SetupOs } from '../setup-command'

const ttlLabels = { '30d': '30 days', '90d': '90 days', '1y': '1 year', never: 'Never expires' } as const

/**
 * Create a token, then show its secret exactly once with a one-line CLI
 * setup command that contains it. The secret lives in this component's state only; closing the
 * dialog drops it and resets the mutation atom.
 */
export function CreateTokenDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const [created, setCreated] = useState<CreatedToken | null>(null)
  const reset = useAtomSet(tokens.create)
  const close = (next: boolean) => {
    if (!next) {
      setCreated(null)
      reset(Atom.Reset)
    }
    onOpenChange(next)
  }
  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-xl">
        {created ? <Reveal token={created} onDone={() => close(false)} /> : <CreateForm onCreated={setCreated} />}
      </DialogContent>
    </Dialog>
  )
}

function CreateForm({ onCreated }: { onCreated: (t: CreatedToken) => void }) {
  const form = useForm({ resolver: schemaResolver(CreateTokenForm), defaultValues: createTokenDefaults })
  const onSubmit = useFormSubmit(form, tokens.create, { onSuccess: onCreated })
  return (
    <>
      <DialogHeader>
        <DialogTitle>New API token</DialogTitle>
        <DialogDescription>Agents and the redrop CLI use API tokens to publish and manage your artifacts.</DialogDescription>
      </DialogHeader>
      <Form {...form}>
        <form onSubmit={onSubmit} className="space-y-4">
          <FormField control={form.control} name="name" render={({ field }) => (
            <FormItem>
              <FormLabel>Name</FormLabel>
              <FormControl><Input placeholder="laptop, ci, build-agent…" autoFocus {...field} /></FormControl>
              <FormDescription>Shown on artifacts this token creates.</FormDescription>
              <FormMessage />
            </FormItem>
          )} />
          <FormField control={form.control} name="ttl" render={({ field }) => (
            <FormItem>
              <FormLabel>Expires after</FormLabel>
              <Select value={field.value} onValueChange={field.onChange}>
                <FormControl><SelectTrigger className="w-full"><SelectValue /></SelectTrigger></FormControl>
                <SelectContent>
                  {Object.entries(ttlLabels).map(([v, label]) => <SelectItem key={v} value={v}>{label}</SelectItem>)}
                </SelectContent>
              </Select>
              <FormMessage />
            </FormItem>
          )} />
          <FormRootError control={form.control} />
          <DialogFooter>
            <Button type="submit" disabled={form.formState.isSubmitting}>Create token</Button>
          </DialogFooter>
        </form>
      </Form>
    </>
  )
}

function Reveal({ token, onDone }: { token: CreatedToken; onDone: () => void }) {
  const config = useAtomSuspense(auth.config).value
  const secret = Redacted.value(token.secret)
  const [os, setOs] = useState<SetupOs>(() => setupOsFromPlatform(navigator.platform))
  return (
    <>
      <DialogHeader>
        <DialogTitle>Token “{token.name}” created</DialogTitle>
      </DialogHeader>
      <SecretReveal label="API token" value={secret}>
        <div className="space-y-2 pt-2">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="text-sm font-medium">Set up the CLI</p>
            <Tabs value={os} onValueChange={(v) => setOs(v as SetupOs)}>
              <TabsList aria-label="Operating system">
                <TabsTrigger value="unix">macOS / Linux</TabsTrigger>
                <TabsTrigger value="windows">Windows</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
          <CopyField value={setupCommand(os, config.public_url, secret)} label="Setup command" />
          <p className="text-muted-foreground text-xs">This command contains the token. It is shown only once and will be saved in your shell history.</p>
          <p className="text-muted-foreground text-xs">
            To keep the token out of history, run the installer without REDROP_TOKEN, then <code>redrop login --url {config.public_url}</code>, which prompts for it.
            Agents can instead use REDROP_URL and REDROP_TOKEN. Run <code>redrop guide</code> for agent instructions.
          </p>
        </div>
      </SecretReveal>
      <DialogFooter>
        <Button onClick={onDone}>I saved it</Button>
      </DialogFooter>
    </>
  )
}
