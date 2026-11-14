import { useForm, useWatch } from 'react-hook-form'
import { FormRootError } from '@/components/form-root-error'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { formatDate } from '@/lib/format'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as artifacts from '../atoms'
import { ArtifactSettingsForm, settingsDefaults, type Artifact } from '../schemas'

const ttlLabels = { keep: 'Keep current expiry', '1h': '1 hour from now', '1d': '1 day from now', '7d': '7 days from now', '30d': '30 days from now', '1y': '1 year from now', never: 'Never expire' } as const
const passwordLabels = { keep: 'Keep current setting', set: 'Set a new password', remove: 'Remove password' } as const

/**
 * Title, expiry, password, view limit, SPA, and the viewer settings. No request shape here: the atom
 * maps form values to the PATCH body (toUpdateRequest) and declares the toast
 * and refresh; server failures render on the form.
 */
export function SettingsForm({ artifact, spa }: { artifact: Artifact; spa: boolean }) {
  const form = useForm({ resolver: schemaResolver(ArtifactSettingsForm), defaultValues: settingsDefaults(artifact, spa) })
  const onSubmit = useFormSubmit(form, artifacts.saveSettings(artifact.id), {
    toArg: (values) => ({ values, kind: artifact.kind }),
    onSuccess: (saved, values) => form.reset(settingsDefaults(saved, values.spa)),
  })
  const passwordAction = useWatch({ control: form.control, name: 'passwordAction' })
  const infoButton = useWatch({ control: form.control, name: 'infoButton' })
  return (
    <Form {...form}>
      <form onSubmit={onSubmit} className="max-w-xl space-y-6">
        <FormField control={form.control} name="title" render={({ field }) => (
          <FormItem>
            <FormLabel>Title</FormLabel>
            <FormControl><Input {...field} /></FormControl>
            <FormMessage />
          </FormItem>
        )} />
        <FormField control={form.control} name="ttl" render={({ field }) => (
          <FormItem>
            <FormLabel>Expiry</FormLabel>
            <Select value={field.value} onValueChange={field.onChange}>
              <FormControl><SelectTrigger className="w-full"><SelectValue /></SelectTrigger></FormControl>
              <SelectContent>
                {Object.entries(ttlLabels).map(([v, label]) => <SelectItem key={v} value={v}>{label}</SelectItem>)}
              </SelectContent>
            </Select>
            <FormDescription>Currently {artifact.expires_at ? `expires ${formatDate(artifact.expires_at)}` : 'never expires'}.</FormDescription>
            <FormMessage />
          </FormItem>
        )} />
        <FormField control={form.control} name="passwordAction" render={({ field }) => (
          <FormItem>
            <FormLabel>Password</FormLabel>
            <Select value={field.value} onValueChange={field.onChange}>
              <FormControl><SelectTrigger className="w-full"><SelectValue /></SelectTrigger></FormControl>
              <SelectContent>
                {Object.entries(passwordLabels)
                  .filter(([v]) => v !== 'remove' || artifact.has_password)
                  .map(([v, label]) => <SelectItem key={v} value={v}>{label}</SelectItem>)}
              </SelectContent>
            </Select>
            <FormDescription>{artifact.has_password ? 'This link is password protected.' : 'Anyone with the link can open it.'} Changing the password signs out current visitors.</FormDescription>
            <FormMessage />
          </FormItem>
        )} />
        {passwordAction === 'set' && (
          <FormField control={form.control} name="password" render={({ field }) => (
            <FormItem>
              <FormLabel>New password</FormLabel>
              <FormControl><PasswordInput autoComplete="new-password" onGenerate={(pw) => form.setValue('password', pw, { shouldDirty: true })} {...field} /></FormControl>
              <FormMessage />
            </FormItem>
          )} />
        )}
        <FormField control={form.control} name="maxViews" render={({ field }) => (
          <FormItem>
            <FormLabel>View limit</FormLabel>
            <FormControl><Input inputMode="numeric" {...field} /></FormControl>
            <FormDescription>0 = unlimited, 1 = one-time link. {artifact.views} view(s) so far.</FormDescription>
            <FormMessage />
          </FormItem>
        )} />
        <FormField control={form.control} name="resetViews" render={({ field }) => (
          <FormItem className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="space-y-0.5">
              <FormLabel>Reset view count</FormLabel>
              <FormDescription>Start counting from zero again (revives a used-up link).</FormDescription>
            </div>
            <FormControl><Switch checked={field.value} onCheckedChange={field.onChange} /></FormControl>
          </FormItem>
        )} />
        {artifact.kind === 'bundle' && (
          <FormField control={form.control} name="spa" render={({ field }) => (
            <FormItem className="flex items-center justify-between gap-4 rounded-lg border p-3">
              <div className="space-y-0.5">
                <FormLabel>Single-page app</FormLabel>
                <FormDescription>Serve the entry page for unknown paths without a file extension.</FormDescription>
              </div>
              <FormControl><Switch checked={field.value} onCheckedChange={field.onChange} /></FormControl>
            </FormItem>
          )} />
        )}
        <FormField control={form.control} name="infoButton" render={({ field }) => (
          <FormItem className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="space-y-0.5">
              <FormLabel>Show info button</FormLabel>
              <FormDescription>
                A small "i" in the corner shows viewers the title, version, expiry, remaining views, size, and the version list with the change notes of new versions. Never who shared it.
                {artifact.kind !== 'file' && ' Pages open in redrop\'s viewer, with the content in a frame; off serves them directly.'}
              </FormDescription>
            </div>
            <FormControl><Switch checked={field.value} onCheckedChange={field.onChange} /></FormControl>
          </FormItem>
        )} />
        <FormField control={form.control} name="oldVersions" render={({ field }) => (
          <FormItem className="flex items-center justify-between gap-4 rounded-lg border p-3">
            <div className="space-y-0.5">
              <FormLabel>Viewers can open older versions</FormLabel>
              <FormDescription>{infoButton ? 'The info panel links each kept version. Off: viewers see the list only.' : 'Needs the info button.'}</FormDescription>
            </div>
            <FormControl><Switch checked={field.value} disabled={!infoButton} onCheckedChange={field.onChange} /></FormControl>
          </FormItem>
        )} />
        <FormRootError control={form.control} />
        <div className="flex gap-2">
          <Button type="submit" disabled={form.formState.isSubmitting || !form.formState.isDirty}>Save changes</Button>
          <Button type="button" variant="ghost" disabled={!form.formState.isDirty} onClick={() => form.reset()}>Discard</Button>
        </div>
      </form>
    </Form>
  )
}
