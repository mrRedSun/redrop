import { useForm } from 'react-hook-form'
import { FormRootError } from '@/components/form-root-error'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as account from '../atoms'
import { PasswordForm, passwordDefaults } from '../schemas'

export function PasswordCard() {
  const form = useForm({ resolver: schemaResolver(PasswordForm), defaultValues: passwordDefaults })
  const onSubmit = useFormSubmit(form, account.changePassword, { onSuccess: () => form.reset(passwordDefaults) })
  return (
    <Card>
      <CardHeader>
        <CardTitle>Password</CardTitle>
        <CardDescription>Changing it signs out your other sessions. Leave “current” empty if you only use single sign-on.</CardDescription>
      </CardHeader>
      <CardContent>
        <Form {...form}>
          <form onSubmit={onSubmit} className="max-w-md space-y-4">
            <FormField control={form.control} name="current" render={({ field }) => (
              <FormItem>
                <FormLabel>Current password</FormLabel>
                <FormControl><Input type="password" autoComplete="current-password" {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <FormField control={form.control} name="password" render={({ field }) => (
              <FormItem>
                <FormLabel>New password</FormLabel>
                <FormControl><PasswordInput autoComplete="new-password" {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <FormField control={form.control} name="confirm" render={({ field }) => (
              <FormItem>
                <FormLabel>Confirm new password</FormLabel>
                <FormControl><Input type="password" autoComplete="new-password" {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <FormRootError control={form.control} />
            <Button type="submit" disabled={form.formState.isSubmitting}>Change password</Button>
          </form>
        </Form>
      </CardContent>
    </Card>
  )
}
