import { Loader2 } from 'lucide-react'
import { cn } from '@/lib/utils'

export function Spinner({ className }: { className?: string }) {
  return <Loader2 className={cn('text-muted-foreground size-5 animate-spin', className)} aria-label="Loading" />
}

export function PageSpinner() {
  return (
    <div className="flex min-h-48 items-center justify-center">
      <Spinner className="size-6" />
    </div>
  )
}
