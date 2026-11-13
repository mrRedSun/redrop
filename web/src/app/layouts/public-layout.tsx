import { Suspense } from 'react'
import { Outlet } from 'react-router'
import { PageSpinner } from '@/components/spinner'

/** Centered card shell for sign-in, setup, invite, and reset pages. */
export function PublicLayout() {
  return (
    <main className="bg-muted/40 flex min-h-svh items-center justify-center p-4">
      <div className="w-full max-w-sm space-y-6">
        <div className="text-center text-xl font-semibold tracking-tight">redrop</div>
        <Suspense fallback={<PageSpinner />}>
          <Outlet />
        </Suspense>
      </div>
    </main>
  )
}
