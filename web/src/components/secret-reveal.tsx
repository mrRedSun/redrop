import { KeyRound } from 'lucide-react'
import type { ReactNode } from 'react'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { CopyField } from './copy-field'

/** A value the server returns once (token secret, invite or reset link). */
export function SecretReveal({ label, value, children }: { label: string; value: string; children?: ReactNode }) {
  return (
    <div className="space-y-3">
      <Alert>
        <KeyRound />
        <AlertTitle>Copy it now</AlertTitle>
        <AlertDescription>This {label.toLowerCase()} is shown only once. Store it somewhere safe.</AlertDescription>
      </Alert>
      <CopyField value={value} label={label} />
      {children}
    </div>
  )
}
