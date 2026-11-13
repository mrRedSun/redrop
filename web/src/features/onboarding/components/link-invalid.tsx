import { Link } from 'react-router'
import { paths } from '@/app/paths'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

export function LinkInvalid({ what }: { what: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>This {what} link is no longer valid</CardTitle>
        <CardDescription>It was already used or has expired. Ask your administrator for a new one.</CardDescription>
      </CardHeader>
      <CardContent>
        <Button asChild variant="outline" className="w-full">
          <Link to={paths.login}>Go to sign in</Link>
        </Button>
      </CardContent>
    </Card>
  )
}
