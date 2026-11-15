import { Badge } from '@/components/ui/badge'
import { type ArtifactStatus, statusLabel } from '../schemas'

const variants: Record<ArtifactStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  active: 'default',
  expired: 'secondary',
  exhausted: 'secondary',
  revoked: 'destructive',
}

export function StatusBadge({ artifact }: { artifact: { readonly status: ArtifactStatus; readonly revoked_by_admin: boolean } }) {
  return <Badge variant={variants[artifact.status]}>{statusLabel(artifact)}</Badge>
}
