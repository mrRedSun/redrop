import { useState } from 'react'
import { Redacted } from 'effect'
import { useForm } from 'react-hook-form'
import { FormRootError } from '@/components/form-root-error'
import { SecretReveal } from '@/components/secret-reveal'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Form, FormControl, FormField, FormItem, FormLabel } from '@/components/ui/form'
import { formatDate } from '@/lib/format'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as admin from '../atoms'
import { ResetLinkForm, type ResetLink, type User } from '../schemas'

/** Creates a 24-hour password reset link and shows it once (no email is sent). */
export function ResetLinkDialog({ user, open, onOpenChange }: { user: User; open: boolean; onOpenChange: (o: boolean) => void }) {
  const [link, setLink] = useState<ResetLink | null>(null)
  const close = (o: boolean) => (o || setLink(null), onOpenChange(o))
  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Password reset link for {user.username}</DialogTitle>
          <DialogDescription>Send the link to the user over a channel you trust. It works once.</DialogDescription>
        </DialogHeader>
        {link ? (
          <>
            <SecretReveal label="Reset link" value={Redacted.value(link.url)}>
              <p className="text-muted-foreground text-xs">Expires {formatDate(link.expires_at)}.</p>
            </SecretReveal>
            <DialogFooter><Button onClick={() => close(false)}>Done</Button></DialogFooter>
          </>
        ) : (
          <CreateForm user={user} onCreated={setLink} />
        )}
      </DialogContent>
    </Dialog>
  )
}

function CreateForm({ user, onCreated }: { user: User; onCreated: (l: ResetLink) => void }) {
  const form = useForm({ resolver: schemaResolver(ResetLinkForm), defaultValues: { clearTotp: false } })
  const onSubmit = useFormSubmit(form, admin.resetLink(user.id), { onSuccess: onCreated })
  return (
    <Form {...form}>
      <form onSubmit={onSubmit} className="space-y-4">
        {user.totp_enabled && (
          <FormField control={form.control} name="clearTotp" render={({ field }) => (
            <FormItem className="flex items-center gap-2">
              <FormControl><Checkbox checked={field.value} onCheckedChange={(v) => field.onChange(v === true)} /></FormControl>
              <FormLabel className="font-normal">Also turn off two-step verification (lost authenticator)</FormLabel>
            </FormItem>
          )} />
        )}
        <FormRootError control={form.control} />
        <DialogFooter><Button type="submit" disabled={form.formState.isSubmitting}>Create link</Button></DialogFooter>
      </form>
    </Form>
  )
}
