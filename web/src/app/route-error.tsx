import { AlertTriangle } from 'lucide-react'
import { Predicate } from 'effect'
import { isRouteErrorResponse, Link, useRouteError } from 'react-router'
import { describeError } from '@/api/errors'
import { Button } from '@/components/ui/button'
import { paths } from './paths'

function describe(error: unknown): { title: string; detail: string; reload?: boolean } {
  if (isRouteErrorResponse(error)) {
    if (error.status === 404) return { title: 'Page not found', detail: 'This page does not exist.' }
    if (error.status === 403) return { title: 'No access', detail: typeof error.data === 'string' ? error.data : 'You cannot open this page.' }
    return { title: `Error ${error.status}`, detail: error.statusText }
  }
  if (Predicate.isTagged(error, 'NotFound')) return { title: 'Not found', detail: 'It may have been deleted, or it belongs to someone else.' }
  if (Predicate.isTagged(error, 'Forbidden')) return { title: 'No access', detail: describeError(error) }
  if (Predicate.isTagged(error, 'Offline') || Predicate.isTagged(error, 'ServerError')) {
    return { title: 'Request failed', detail: describeError(error), reload: true }
  }
  // A failed dynamic import usually means a new version was deployed.
  if (error instanceof TypeError && /dynamically imported module|Importing a module script failed/i.test(error.message)) {
    return { title: 'Update available', detail: 'The dashboard was updated. Reload to continue.', reload: true }
  }
  return { title: 'Something went wrong', detail: describeError(error), reload: true }
}

/** Root error boundary: guard 403s, failed loaders, lazy chunk failures, render crashes, suspended atom failures. */
export function RouteError() {
  const { title, detail, reload } = describe(useRouteError())
  return (
    <div className="mx-auto flex max-w-md flex-col items-center gap-3 px-6 py-24 text-center">
      <AlertTriangle className="text-muted-foreground size-10" aria-hidden />
      <h1 className="text-xl font-semibold">{title}</h1>
      <p className="text-muted-foreground text-sm">{detail}</p>
      <div className="flex gap-2 pt-2">
        <Button asChild variant="outline">
          <Link to={paths.artifacts}>Go to artifacts</Link>
        </Button>
        {reload && <Button onClick={() => window.location.reload()}>Reload</Button>}
      </div>
    </div>
  )
}

export function NotFoundPage() {
  return (
    <div className="mx-auto flex max-w-md flex-col items-center gap-3 px-6 py-24 text-center">
      <h1 className="text-xl font-semibold">Page not found</h1>
      <p className="text-muted-foreground text-sm">This page does not exist.</p>
      <Button asChild variant="outline">
        <Link to={paths.artifacts}>Go to artifacts</Link>
      </Button>
    </div>
  )
}
