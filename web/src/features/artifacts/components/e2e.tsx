import { KeyRound } from 'lucide-react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'

/**
 * End-to-end encrypted artifacts (DESIGN.md "End-to-end encrypted links").
 * The server, and so the dashboard, has only ciphertext and never the key,
 * which lives in the link's #k= fragment the CLI printed.
 */
export function E2EBadge() {
  return (
    <Badge variant="outline" title="End-to-end encrypted: the server stores only ciphertext">
      <KeyRound aria-hidden />e2e
    </Badge>
  )
}

/** The dashboard's link has no key, so it opens nothing on its own. */
export function E2ELinkNote({ id }: { id: string }) {
  return (
    <p className="text-muted-foreground text-sm">
      End-to-end encrypted: this link has no key and opens nothing by itself. The working link ends in{' '}
      <code>#k=…</code> and was printed by <code>redrop share --e2e</code>; the server never had the key.{' '}
      <code>redrop info {id}</code> prints it on the machine that shared it.
    </p>
  )
}

export function E2EPreviewNote() {
  return (
    <Alert>
      <KeyRound />
      <AlertDescription>
        This artifact is end-to-end encrypted, so the server can't show a preview. Open it with its full link.
      </AlertDescription>
    </Alert>
  )
}

export function E2EFilesNote() {
  return (
    <p className="text-muted-foreground pb-3 text-sm">
      File names and types are encrypted. These are the stored objects: an encrypted manifest and one object per file.
    </p>
  )
}
