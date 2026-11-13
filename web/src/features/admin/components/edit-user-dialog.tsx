import { useForm } from 'react-hook-form'
import { FormRootError } from '@/components/form-root-error'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { schemaResolver, useFormSubmit } from '@/lib/forms'
import * as admin from '../atoms'
import { EditUserForm, editUserDefaults, type User } from '../schemas'

/** Role, quota, display name, disable. The server refuses to demote or disable yourself or the last admin. */
export function EditUserDialog({ user, open, onOpenChange }: { user: User; open: boolean; onOpenChange: (o: boolean) => void }) {
  const form = useForm({ resolver: schemaResolver(EditUserForm), defaultValues: editUserDefaults(user) })
  const onSubmit = useFormSubmit(form, admin.updateUser(user.id), { onSuccess: () => onOpenChange(false) })
  return (
    <Dialog open={open} onOpenChange={(o) => (o || form.reset(editUserDefaults(user)), onOpenChange(o))}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Edit {user.username}</DialogTitle>
          <DialogDescription>Disabling a user signs them out and stops their API tokens.</DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form onSubmit={onSubmit} className="space-y-4">
            <FormField control={form.control} name="displayName" render={({ field }) => (
              <FormItem>
                <FormLabel>Display name</FormLabel>
                <FormControl><Input {...field} /></FormControl>
                <FormMessage />
              </FormItem>
            )} />
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
                <FormMessage />
              </FormItem>
            )} />
            <FormField control={form.control} name="quotaGB" render={({ field }) => (
              <FormItem>
                <FormLabel>Storage quota (GB)</FormLabel>
                <FormControl><Input inputMode="decimal" {...field} /></FormControl>
                <FormDescription>0 = only the server total applies.</FormDescription>
                <FormMessage />
              </FormItem>
            )} />
            <FormField control={form.control} name="disabled" render={({ field }) => (
              <FormItem className="flex items-center justify-between rounded-lg border p-3">
                <FormLabel>Disabled</FormLabel>
                <FormControl><Switch checked={field.value} onCheckedChange={field.onChange} /></FormControl>
              </FormItem>
            )} />
            <FormRootError control={form.control} />
            <DialogFooter>
              <Button type="submit" disabled={form.formState.isSubmitting || !form.formState.isDirty}>Save</Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}
