import type { ReactNode } from 'react'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Progress } from '@/components/ui/progress'

type Props = { label: string; value: ReactNode; hint?: ReactNode; /** 0-100 */ progress?: number }

export function StatCard({ label, value, hint, progress }: Props) {
  return (
    <Card className="gap-2">
      <CardHeader>
        <CardDescription>{label}</CardDescription>
        <CardTitle className="text-2xl tabular-nums">{value}</CardTitle>
      </CardHeader>
      {(hint || progress !== undefined) && (
        <CardContent className="space-y-2">
          {progress !== undefined && <Progress aria-label={label} value={Math.min(100, Math.max(0, progress))} />}
          {hint && <p className="text-muted-foreground text-xs">{hint}</p>}
        </CardContent>
      )}
    </Card>
  )
}
