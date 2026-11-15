import { useState } from 'react'
import { useAtomSet, useAtomValue } from '@effect/atom-react'
import { MessageSquare } from 'lucide-react'
import { AsyncResultView } from '@/components/async-result-view'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { EmptyState } from '@/components/empty-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { formatDate, relativeTime } from '@/lib/format'
import * as artifacts from '../atoms'
import { groupByVersion, type Artifact, type Comment } from '../schemas'

/**
 * Viewer comments, grouped by version. Every field is untrusted viewer text
 * and is rendered as plain React text only (never HTML or Markdown).
 */
export function FeedbackPanel({ artifact }: { artifact: Artifact }) {
  const [showResolved, setShowResolved] = useState(false)
  const result = useAtomValue(artifacts.feedback(artifacts.feedbackKey(artifact.id, showResolved ? 'all' : 'open')))
  const resolve = useAtomSet(artifacts.resolveComment(artifact.id))
  const remove = useAtomSet(artifacts.deleteComment(artifact.id), { mode: 'promise' })
  const [deleting, setDeleting] = useState<string | null>(null)
  const open = artifact.feedback && artifact.info_button
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-muted-foreground max-w-2xl text-sm">
          {open
            ? 'Viewers can comment from the info panel. '
            : artifact.feedback
              ? 'Comments are closed while the info button is off. '
              : 'Comments are off; turn on viewer feedback in Settings. '}
          Comments are untrusted text from anyone who could open the link. Only you see them.
        </p>
        <div className="flex items-center gap-2">
          <Switch id="feedback-resolved" checked={showResolved} onCheckedChange={setShowResolved} />
          <Label htmlFor="feedback-resolved">Show resolved</Label>
        </div>
      </div>
      <AsyncResultView result={result}>
        {(comments) =>
          comments.length === 0 ? (
            <EmptyState
              icon={MessageSquare}
              title={showResolved ? 'No comments' : 'No open comments'}
              hint={open ? 'Comments viewers send from the info panel appear here.' : 'Turn on viewer feedback (with the info button) to let viewers comment.'}
            />
          ) : (
            <div className="space-y-6">
              {groupByVersion(comments).map((group) => (
                <section key={group.version} className="space-y-3">
                  <h3 className="flex items-center gap-2 text-sm font-semibold">
                    Version {group.version}
                    {group.version === artifact.version && <Badge>live</Badge>}
                    <span className="text-muted-foreground font-normal">· {group.comments.length} comment(s)</span>
                  </h3>
                  {group.comments.map((c) => (
                    <CommentCard
                      key={c.id}
                      comment={c}
                      onResolve={(resolved) => resolve({ comment: c.id, resolved })}
                      onDelete={() => setDeleting(c.id)}
                    />
                  ))}
                </section>
              ))}
            </div>
          )
        }
      </AsyncResultView>
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(o) => !o && setDeleting(null)}
        title="Delete this comment?"
        description="It is removed permanently."
        confirmLabel="Delete comment"
        destructive
        onConfirm={() => (deleting === null ? undefined : remove(deleting))}
      />
    </div>
  )
}

function CommentCard({ comment: c, onResolve, onDelete }: { comment: Comment; onResolve: (resolved: boolean) => void; onDelete: () => void }) {
  const resolved = c.resolved_at !== null
  return (
    <article className={`rounded-lg border p-4 ${resolved ? 'opacity-70' : ''}`}>
      <header className="mb-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm">
        <span className="font-medium">{c.name || 'Anonymous'}</span>
        <span className="text-muted-foreground" title={formatDate(c.created_at)}>{relativeTime(c.created_at)}</span>
        <code className="bg-muted max-w-full truncate rounded px-1.5 py-0.5 text-xs" title={c.page}>{c.page}</code>
        {resolved && <Badge variant="outline">resolved {relativeTime(c.resolved_at)}</Badge>}
      </header>
      {c.quote && (
        <blockquote className="border-primary/40 text-muted-foreground mb-2 border-l-2 pl-3 text-sm whitespace-pre-wrap [overflow-wrap:anywhere]">
          <span className="opacity-60">{c.quote.prefix}</span>
          <mark className="text-foreground bg-transparent font-medium">{c.quote.exact}</mark>
          <span className="opacity-60">{c.quote.suffix}</span>
        </blockquote>
      )}
      <p className="text-sm whitespace-pre-wrap [overflow-wrap:anywhere]">{c.body}</p>
      <div className="mt-3 flex gap-2">
        <Button size="sm" variant="outline" onClick={() => onResolve(!resolved)}>{resolved ? 'Reopen' : 'Resolve'}</Button>
        <Button size="sm" variant="ghost" onClick={onDelete}>Delete</Button>
      </div>
    </article>
  )
}
