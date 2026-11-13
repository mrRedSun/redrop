import { useAtomRefresh, useAtomValue } from '@effect/atom-react'
import { RefreshCw } from 'lucide-react'
import { AsyncResultView } from '@/components/async-result-view'
import { Button } from '@/components/ui/button'
import * as artifacts from '../atoms'

/**
 * Owner preview through a short-lived signed link: bypasses the password and
 * view limit and never counts as a view. The content runs on its own origin;
 * the iframe sandbox keeps it from navigating the dashboard.
 */
export function PreviewPanel({ id }: { id: string }) {
  const result = useAtomValue(artifacts.preview(id))
  const refresh = useAtomRefresh(artifacts.preview(id))
  return (
    <AsyncResultView result={result}>
      {(link) => (
        <div className="space-y-2">
          <div className="flex items-center justify-between gap-2">
            <p className="text-muted-foreground text-sm">Previews do not count as views and skip the password.</p>
            <Button variant="outline" size="sm" onClick={refresh}>
              <RefreshCw className="size-4" /> Reload
            </Button>
          </div>
          <iframe
            key={link.url}
            src={link.url}
            title="Artifact preview"
            className="h-[70vh] w-full rounded-lg border bg-white"
            sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads"
            referrerPolicy="no-referrer"
          />
        </div>
      )}
    </AsyncResultView>
  )
}
