import { Suspense } from 'react'
import { NavLink, Outlet } from 'react-router'
import { paths } from '@/app/paths'
import { PageHeader } from '@/components/page-header'
import { PageSpinner } from '@/components/spinner'
import { cn } from '@/lib/utils'

const tabs = [
  { to: paths.admin, label: 'Overview', end: true },
  { to: paths.adminUsers, label: 'Users' },
  { to: paths.adminInvites, label: 'Invites' },
  { to: paths.adminArtifacts, label: 'Artifacts' },
]

/** Admin section (guarded by requireAdmin). Admins see artifact metadata only, never links or content. */
export function Component() {
  return (
    <>
      <PageHeader title="Administration" />
      <nav className="mb-6 flex gap-1 border-b">
        {tabs.map((t) => (
          <NavLink
            key={t.to}
            to={t.to}
            end={t.end}
            className={({ isActive }) =>
              cn('-mb-px border-b-2 px-3 py-2 text-sm font-medium', isActive ? 'border-primary text-foreground' : 'text-muted-foreground hover:text-foreground border-transparent')
            }
          >
            {t.label}
          </NavLink>
        ))}
      </nav>
      <Suspense fallback={<PageSpinner />}>
        <Outlet />
      </Suspense>
    </>
  )
}
