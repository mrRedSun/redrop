import { useAtomSuspense } from '@effect/atom-react'
import { useForm } from 'react-hook-form'
import { useParams } from 'react-router'
import { paths } from '@/app/paths'
import { FormRootError } from '@/components/form-root-error'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as onboarding from '../atoms'
import { LinkInvalid } from '../components/link-invalid'
import { NewPasswordForm, newPasswordDefaults } from '../schemas'

/** Password reset from an admin-created link; every session of the user ends. */
export function Component() {
  const token = useParams().token ?? ''
  const info = useAtomSuspense(onboarding.resetInfo(token)).value
  const form = useForm({ resolver: schemaResolver(NewPasswordForm), defaultValues: newPasswordDefaults })
  const onSubmit = useFormSubmit(form, onboarding.resetPassword(token), { onSuccess: () => window.location.replace(paths.login) })
  if (!info.valid) return <LinkInvalid what="password reset" />
  return (
    <Card>
      <CardHeader>
        <CardTitle>Choose a new password</CardTitle>
        <CardDescription>For {info.username}. You will be signed out everywhere.</CardDescription>
      </CardHeader>
      <CardContent>
        <Form {...form}>
          <form onSubmit={onSubmit} className="space-y-4">
            <FormField control={form.control} name="password" render={({ field }) => (
              <FormItem>
                <FormLabel>New password</FormLabel>
                <FormControl><PasswordInput autoComplete="new-password" autoFocus {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <FormField control={form.control} name="confirm" render={({ field }) => (
              <FormItem>
                <FormLabel>Confirm password</FormLabel>
                <FormControl><Input type="password" autoComplete="new-password" {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <FormRootError control={form.control} />
            <Button type="submit" className="w-full" disabled={form.formState.isSubmitting}>Set password</Button>
          </form>
        </Form>
      </CardContent>
    </Card>
  )
}
