import { useState } from 'react'
import { useAtomSet, useAtomSuspense } from '@effect/atom-react'
import { Redacted } from 'effect'
import { Atom } from 'effect/reactivity'
import { useForm } from 'react-hook-form'
import { FormRootError } from '@/components/form-root-error'
import { SecretReveal } from '@/components/secret-reveal'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import * as auth from '@/features/auth/atoms'
import { formatDate } from '@/lib/format'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as admin from '../atoms'
import { CreateInviteForm, createInviteDefaults, type CreatedInvite } from '../schemas'

const ttlLabels = { '1d': '1 day', '7d': '7 days', '30d': '30 days' } as const

/** Creates an invite link and shows it once. */
export function CreateInviteDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const [created, setCreated] = useState<CreatedInvite | null>(null)
  const reset = useAtomSet(admin.createInvite)
  const close = (o: boolean) => {
    if (!o) {
      setCreated(null)
      reset(Atom.Reset)
    }
    onOpenChange(o)
  }
  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Invite someone</DialogTitle>
          <DialogDescription>The link creates one account. Share it only with someone you trust.</DialogDescription>
        </DialogHeader>
        {created ? (
          <>
            <SecretReveal label="Invite link" value={Redacted.value(created.url)}>
              <p className="text-muted-foreground text-xs">{created.role === 'admin' ? 'Administrator invite. ' : ''}Expires {formatDate(created.expires_at)}.</p>
              {created.email && <p className="text-muted-foreground text-xs">Invite email to {created.email}. Delivery status is shown in the invite list.</p>}
            </SecretReveal>
            <DialogFooter><Button onClick={() => close(false)}>Done</Button></DialogFooter>
          </>
        ) : (
          <InviteForm onCreated={setCreated} />
        )}
      </DialogContent>
    </Dialog>
  )
}

export function InviteForm({ onCreated }: { onCreated: (i: CreatedInvite) => void }) {
  const config = useAtomSuspense(auth.config).value
  const form = useForm({ resolver: schemaResolver(CreateInviteForm), defaultValues: createInviteDefaults })
  const onSubmit = useFormSubmit(form, admin.createInvite, { onSuccess: onCreated })
  return (
    <Form {...form}>
      <form onSubmit={onSubmit} className="space-y-4" noValidate>
        {config.mail && <FormField control={form.control} name="email" render={({ field }) => (
          <FormItem>
            <FormLabel>Email (optional)</FormLabel>
            <FormControl><Input type="email" autoComplete="email" maxLength={254} placeholder="recipient@example.com" {...field} /></FormControl>
            <FormDescription>Email the invite link, or leave empty to share it yourself.</FormDescription>
            <FormMessage />
          </FormItem>
        )} />}
        <FormField control={form.control} name="note" render={({ field }) => (
          <FormItem>
            <FormLabel>Note</FormLabel>
            <FormControl><Input placeholder="Who is this for?" autoFocus {...field} /></FormControl>
            <FormDescription>{config.mail ? 'Included in the email if you enter a recipient. Admins can also see it.' : 'Only admins see it.'}</FormDescription>
            <FormMessage />
          </FormItem>
        )} />
        <div className="grid gap-4 sm:grid-cols-3">
          <FormField control={form.control} name="role" render={({ field }) => (
            <FormItem>
              <FormLabel>Role</FormLabel>
              <Select value={field.value} onValueChange={field.onChange}>
                <FormControl><SelectTrigger className="w-full"><SelectValue /></SelectTrigger></FormControl>
                <SelectContent>
                  <SelectItem value="user">User</SelectItem>
                  <SelectItem value="admin">Admin</SelectItem>
                </SelectContent>
              </Select>
            </FormItem>
          )} />
          <FormField control={form.control} name="quotaGB" render={({ field }) => (
            <FormItem>
              <FormLabel>Quota (GB)</FormLabel>
              <FormControl><Input inputMode="decimal" placeholder="default" {...field} /></FormControl>
              <FormMessage />
            </FormItem>
          )} />
          <FormField control={form.control} name="ttl" render={({ field }) => (
            <FormItem>
              <FormLabel>Valid for</FormLabel>
              <Select value={field.value} onValueChange={field.onChange}>
                <FormControl><SelectTrigger className="w-full"><SelectValue /></SelectTrigger></FormControl>
                <SelectContent>
                  {Object.entries(ttlLabels).map(([v, l]) => <SelectItem key={v} value={v}>{l}</SelectItem>)}
                </SelectContent>
              </Select>
            </FormItem>
          )} />
        </div>
        <FormRootError control={form.control} />
        <DialogFooter><Button type="submit" disabled={form.formState.isSubmitting}>Create invite</Button></DialogFooter>
      </form>
    </Form>
  )
}
