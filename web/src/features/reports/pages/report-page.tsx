import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { CircleCheck } from 'lucide-react'
import { FormRootError } from '@/components/form-root-error'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as reports from '../atoms'
import { ReportForm, categoryLabels, maxContact, maxDetails, reportDefaults } from '../schemas'

/**
 * Public abuse report form. Pages on the content domain link here with the
 * page in the fragment; the form never shows whether a link exists.
 */
export function Component() {
  const [sent, setSent] = useState(false)
  const form = useForm({ resolver: schemaResolver(ReportForm), defaultValues: reportDefaults(window.location.hash) })
  const onSubmit = useFormSubmit(form, reports.submit, { onSuccess: () => setSent(true) })
  if (sent) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2"><CircleCheck className="size-5" aria-hidden />Thank you</CardTitle>
          <CardDescription>The report was sent to the people who run this server. They review every report before acting on it.</CardDescription>
        </CardHeader>
      </Card>
    )
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle>Report abuse</CardTitle>
        <CardDescription>Tell the people who run this server about a shared page that is harmful or illegal. You don't need an account.</CardDescription>
      </CardHeader>
      <CardContent>
        <Form {...form}>
          <form onSubmit={onSubmit} className="space-y-4">
            <FormField control={form.control} name="url" render={({ field }) => (
              <FormItem>
                <FormLabel>Address of the page</FormLabel>
                <FormControl><Input type="url" inputMode="url" autoComplete="off" spellCheck={false} {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <FormField control={form.control} name="category" render={({ field }) => (
              <FormItem>
                <FormLabel>What is wrong</FormLabel>
                <Select value={field.value} onValueChange={field.onChange}>
                  <FormControl><SelectTrigger className="w-full"><SelectValue /></SelectTrigger></FormControl>
                  <SelectContent>
                    {Object.entries(categoryLabels).map(([v, l]) => <SelectItem key={v} value={v}>{l}</SelectItem>)}
                  </SelectContent>
                </Select>
              </FormItem>
            )} />
            <FormField control={form.control} name="details" render={({ field }) => (
              <FormItem>
                <FormLabel>Details <span className="text-muted-foreground font-normal">(optional)</span></FormLabel>
                <FormControl><Textarea rows={5} maxLength={maxDetails * 2} {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
            <FormField control={form.control} name="contact" render={({ field }) => (
              <FormItem>
                <FormLabel>Your contact <span className="text-muted-foreground font-normal">(optional)</span></FormLabel>
                <FormControl><Input autoComplete="email" maxLength={maxContact * 2} {...field} /></FormControl>
                <FormDescription>Only if you are happy to be asked about this report.</FormDescription>
                <FormMessage />
              </FormItem>
            )} />
            <FormRootError control={form.control} />
            <Button type="submit" className="w-full" disabled={form.formState.isSubmitting}>Send report</Button>
          </form>
        </Form>
      </CardContent>
    </Card>
  )
}
