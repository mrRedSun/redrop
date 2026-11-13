import { Badge } from '@/components/ui/badge'
import type { ArtifactStatus } from '../schemas'

const variants: Record<ArtifactStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  active: 'default',
  expired: 'secondary',
  exhausted: 'secondary',
  revoked: 'destructive',
}

export function StatusBadge({ status }: { status: ArtifactStatus }) {
  return <Badge variant={variants[status]}>{status}</Badge>
}
