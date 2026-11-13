import { Eye, EyeOff, Wand2 } from 'lucide-react'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { generatePassword } from '@/lib/format'

type Props = Omit<React.ComponentProps<typeof Input>, 'type'> & {
  /** Offer a "generate" button that fills a random password (and reveals it). */
  onGenerate?: (password: string) => void
}

export function PasswordInput({ onGenerate, className, ...props }: Props) {
  const [visible, setVisible] = useState(false)
  return (
    <div className="flex items-center gap-2">
      <Input {...props} type={visible ? 'text' : 'password'} className={className} />
      <Button type="button" variant="outline" size="icon" onClick={() => setVisible((v) => !v)} aria-label={visible ? 'Hide password' : 'Show password'}>
        {visible ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
      </Button>
      {onGenerate && (
        <Button
          type="button"
          variant="outline"
          size="icon"
          aria-label="Generate password"
          onClick={() => {
            onGenerate(generatePassword())
            setVisible(true)
          }}
        >
          <Wand2 className="size-4" />
        </Button>
      )}
    </div>
  )
}
