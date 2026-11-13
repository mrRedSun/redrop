import { AlertCircle } from 'lucide-react'
import { useFormState, type Control, type FieldValues } from 'react-hook-form'
import { Alert, AlertDescription } from '@/components/ui/alert'

/** Renders a submission error set by useFormSubmit(). */
export function FormRootError<T extends FieldValues, U>({ control }: { control: Control<T, unknown, U> }) {
  const { errors } = useFormState({ control })
  const message = errors.root?.server?.message
  if (!message) return null
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertDescription>{message}</AlertDescription>
    </Alert>
  )
}
