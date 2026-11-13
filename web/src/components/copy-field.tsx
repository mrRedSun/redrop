import { Input } from '@/components/ui/input'
import { CopyButton } from './copy-button'

/** Read-only value with a copy button (links, commands, secrets). */
export function CopyField({ value, label, mono = true }: { value: string; label?: string; mono?: boolean }) {
  return (
    <div className="flex items-center gap-2">
      <Input readOnly value={value} aria-label={label} className={mono ? 'font-mono text-xs' : undefined} onFocus={(e) => e.currentTarget.select()} />
      <CopyButton value={value} label={label ? `Copy ${label.toLowerCase()}` : 'Copy'} />
    </div>
  )
}
