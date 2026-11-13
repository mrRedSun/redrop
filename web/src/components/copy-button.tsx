import { Check, Copy } from 'lucide-react'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { useCopy } from '@/hooks/use-copy'

type Props = { value: string; label?: string; size?: 'icon' | 'sm' }

export function CopyButton({ value, label = 'Copy', size = 'icon' }: Props) {
  const copy = useCopy()
  const [copied, setCopied] = useState(false)
  const onClick = async () => {
    if (!(await copy(value))) return
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  const Icon = copied ? Check : Copy
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button type="button" variant="outline" size={size} onClick={onClick} aria-label={label}>
          <Icon className="size-4" />
          {size === 'sm' && label}
        </Button>
      </TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  )
}
