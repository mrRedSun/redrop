import type { LucideIcon } from 'lucide-react'
import type { ReactNode } from 'react'

type Props = { icon?: LucideIcon; title: string; hint?: ReactNode; action?: ReactNode }

export function EmptyState({ icon: Icon, title, hint, action }: Props) {
  return (
    <div className="flex flex-col items-center gap-2 rounded-lg border border-dashed px-6 py-12 text-center">
      {Icon && <Icon className="text-muted-foreground size-8" aria-hidden />}
      <p className="font-medium">{title}</p>
      {hint && <p className="text-muted-foreground max-w-md text-sm">{hint}</p>}
      {action && <div className="pt-2">{action}</div>}
    </div>
  )
}
