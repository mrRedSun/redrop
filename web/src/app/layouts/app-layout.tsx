import { Suspense } from 'react'
import { useAtomSuspense } from '@effect/atom-react'
import { Option } from 'effect'
import { NavLink, Outlet } from 'react-router'
import { FileStack, KeyRound, Menu, Shield, UserRound } from 'lucide-react'
import { PageSpinner } from '@/components/spinner'
import { Button } from '@/components/ui/button'
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from '@/components/ui/sheet'
import * as auth from '@/features/auth/atoms'
import { cn } from '@/lib/utils'
import { paths } from '../paths'
import { UserMenu } from './user-menu'

const nav = [
  { to: paths.artifacts, label: 'Artifacts', icon: FileStack },
  { to: paths.tokens, label: 'API tokens', icon: KeyRound },
  { to: paths.account, label: 'Account', icon: UserRound },
]

function NavItems({ admin }: { admin: boolean }) {
  const items = admin ? [...nav, { to: paths.admin, label: 'Admin', icon: Shield }] : nav
  return items.map(({ to, label, icon: Icon }) => (
    <NavLink
      key={to}
      to={to}
      className={({ isActive }) =>
        cn(
          'text-muted-foreground hover:text-foreground flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition-colors',
          isActive && 'bg-accent text-foreground',
        )
      }
    >
      <Icon className="size-4" />
      {label}
    </NavLink>
  ))
}

/**
 * Shell for signed-in pages. requireSession guards the first render; a
 * session that ends later is handled by session-watch (full reload to login).
 */
export function AppLayout() {
  const me = Option.getOrUndefined(useAtomSuspense(auth.session).value)
  const admin = !!me?.can_admin
  return (
    <div className="min-h-svh">
      <header className="bg-background/95 sticky top-0 z-40 border-b backdrop-blur">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-4 px-4">
          <Sheet>
            <SheetTrigger asChild>
              <Button variant="ghost" size="icon" className="md:hidden" aria-label="Open navigation">
                <Menu className="size-5" />
              </Button>
            </SheetTrigger>
            <SheetContent side="left" className="w-64 p-4">
              <SheetTitle className="px-3 pb-2">redrop</SheetTitle>
              <nav className="flex flex-col gap-1">
                <NavItems admin={admin} />
              </nav>
            </SheetContent>
          </Sheet>
          <NavLink to={paths.artifacts} className="font-semibold tracking-tight">
            redrop
          </NavLink>
          <nav className="hidden items-center gap-1 md:flex">
            <NavItems admin={admin} />
          </nav>
          <div className="ml-auto">{me && <UserMenu me={me} />}</div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-8">
        <Suspense fallback={<PageSpinner />}>
          <Outlet />
        </Suspense>
      </main>
    </div>
  )
}
