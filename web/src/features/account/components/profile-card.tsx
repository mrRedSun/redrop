import { useForm } from 'react-hook-form'
import { FormRootError } from '@/components/form-root-error'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import type { Me } from '@/features/auth/schemas'
import { formatBytes } from '@/lib/format'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as account from '../atoms'
import { ProfileForm } from '../schemas'

export function ProfileCard({ me }: { me: Me }) {
  const form = useForm({ resolver: schemaResolver(ProfileForm), defaultValues: { displayName: me.user.display_name } })
  const onSubmit = useFormSubmit(form, account.updateProfile, { onSuccess: (_, v) => form.reset({ displayName: v.displayName }) })
  const quota = me.user.quota_bytes > 0 ? `${formatBytes(me.user.used_bytes)} of ${formatBytes(me.user.quota_bytes)}` : formatBytes(me.user.used_bytes)
  return (
    <Card>
      <CardHeader>
        <CardTitle>Profile</CardTitle>
        <CardDescription>
          Signed in as <strong>{me.user.username}</strong> ({me.user.role}) · {me.user.artifacts} artifact(s) using {quota}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Form {...form}>
          <form onSubmit={onSubmit} className="flex flex-wrap items-start gap-2">
            <FormField control={form.control} name="displayName" render={({ field }) => (
              <FormItem className="min-w-60 flex-1">
                <FormLabel className="sr-only">Display name</FormLabel>
                <FormControl><Input placeholder="Display name" {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <Button type="submit" disabled={form.formState.isSubmitting || !form.formState.isDirty}>Save</Button>
            <div className="basis-full"><FormRootError control={form.control} /></div>
          </form>
        </Form>
      </CardContent>
    </Card>
  )
}
