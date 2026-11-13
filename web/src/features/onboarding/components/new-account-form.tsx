import { useForm } from 'react-hook-form'
import type { Atom } from 'effect/reactivity'
import { FormRootError } from '@/components/form-root-error'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import { NewAccountForm, newAccountDefaults, type NewAccount } from '../schemas'

type Props<A, E> = { submit: Atom.AtomResultFn<NewAccount, A, E>; label: string; onDone: () => void }

/** Username + password + confirmation; shared by first-run setup and invite acceptance. */
export function NewAccountFields<A, E>({ submit, label, onDone }: Props<A, E>) {
  const form = useForm({ resolver: schemaResolver(NewAccountForm), defaultValues: newAccountDefaults })
  const onSubmit = useFormSubmit(form, submit, { onSuccess: onDone })
  return (
    <Form {...form}>
      <form onSubmit={onSubmit} className="space-y-4">
        <FormField control={form.control} name="username" render={({ field }) => (
          <FormItem>
            <FormLabel>Username</FormLabel>
            <FormControl><Input autoComplete="username" autoCapitalize="none" autoFocus {...field} /></FormControl>
            <FormMessage />
          </FormItem>
        )} />
        <FormField control={form.control} name="password" render={({ field }) => (
          <FormItem>
            <FormLabel>Password</FormLabel>
            <FormControl><PasswordInput autoComplete="new-password" {...field} /></FormControl>
            <FormDescription>At least 10 characters.</FormDescription>
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
        <Button type="submit" className="w-full" disabled={form.formState.isSubmitting}>{label}</Button>
      </form>
    </Form>
  )
}
