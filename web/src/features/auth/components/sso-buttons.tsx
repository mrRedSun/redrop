import { LogIn } from 'lucide-react'
import { Button } from '@/components/ui/button'
import type { AuthProvider } from '../schemas'
import { ssoStartUrl } from '../atoms'

type Props = { providers: ReadonlyArray<AuthProvider>; mode: 'login' | 'link' | 'invite'; inviteToken?: string; label?: string }

/** Single sign-on starts with a browser navigation (the server redirects to the IdP), not a fetch. */
export function SsoButtons({ providers, mode, inviteToken, label = 'Continue with' }: Props) {
  if (providers.length === 0) return null
  return (
    <div className="grid gap-2">
      {providers.map((p) => (
        <Button key={p.id} variant="outline" asChild>
          <a href={ssoStartUrl(p.id, mode, inviteToken)}>
            <LogIn className="size-4" /> {label} {p.label}
          </a>
        </Button>
      ))}
    </div>
  )
}
