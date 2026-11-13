import { useAtomSuspense, useAtomValue } from '@effect/atom-react'
import { ArrowLeft, ExternalLink } from 'lucide-react'
import { Link, useParams } from 'react-router'
import { paths } from '@/app/paths'
import { CopyField } from '@/components/copy-field'
import { AsyncResultView } from '@/components/async-result-view'
import { PageHeader } from '@/components/page-header'
import { StatCard } from '@/components/stat-card'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { formatBytes, formatDate, relativeTime } from '@/lib/format'
import * as artifacts from '../atoms'
import { AccessLogPanel } from '../components/access-log-panel'
import { FilesPanel } from '../components/files-panel'
import { LinkActions } from '../components/link-actions'
import { PreviewPanel } from '../components/preview-panel'
import { SettingsForm } from '../components/settings-form'
import { StatusBadge } from '../components/status-badge'
import { VersionsPanel } from '../components/versions-panel'
import type { Artifact } from '../schemas'

/** One artifact: link, stats, settings, versions, files, access log, preview. */
export function Component() {
  const id = useParams().id ?? ''
  // Suspends until loaded; a NotFound lands in the route error boundary.
  const artifact = useAtomSuspense(artifacts.detail(id)).value
  const versions = useAtomValue(artifacts.versions(id))

  return (
    <>
      <Button asChild variant="ghost" size="sm" className="mb-2 -ml-2">
        <Link to={paths.artifacts}><ArrowLeft className="size-4" /> Artifacts</Link>
      </Button>
      <PageHeader
        title={<span className="flex items-center gap-3">{artifact.title || artifact.id}<StatusBadge status={artifact.status} /></span>}
        description={`${artifact.kind} · created ${formatDate(artifact.created_at)}${artifact.token_name ? ` via ${artifact.token_name}` : ''}`}
        actions={<LinkActions artifact={artifact} afterDelete="list" />}
      />
      <Card className="mb-6">
        <CardHeader><CardTitle className="text-base">Share link</CardTitle></CardHeader>
        <CardContent className="flex items-center gap-2">
          <div className="flex-1"><CopyField value={artifact.url} label="Link" /></div>
          <Button asChild variant="outline" size="icon" aria-label="Open link">
            <a href={artifact.url} target="_blank" rel="noreferrer noopener"><ExternalLink className="size-4" /></a>
          </Button>
        </CardContent>
      </Card>
      <Stats artifact={artifact} />
      <Tabs defaultValue="settings" className="mt-6">
        <TabsList>
          <TabsTrigger value="settings">Settings</TabsTrigger>
          <TabsTrigger value="versions">Versions ({artifact.versions})</TabsTrigger>
          <TabsTrigger value="files">Files ({artifact.files})</TabsTrigger>
          <TabsTrigger value="log">Access log</TabsTrigger>
          <TabsTrigger value="preview">Preview</TabsTrigger>
        </TabsList>
        <TabsContent value="settings" className="pt-4">
          {/* The SPA flag belongs to the live version; wait for it so the switch starts correct. */}
          <AsyncResultView result={versions}>
            {(items) => <SettingsForm key={`${artifact.id}-${artifact.version}`} artifact={artifact} spa={items.find((v) => v.current)?.spa ?? false} />}
          </AsyncResultView>
        </TabsContent>
        <TabsContent value="versions" className="pt-4"><VersionsPanel id={id} /></TabsContent>
        <TabsContent value="files" className="pt-4"><AsyncResultView result={versions}>{(items) => <FilesPanel id={id} versions={items} />}</AsyncResultView></TabsContent>
        <TabsContent value="log" className="pt-4"><AccessLogPanel id={id} /></TabsContent>
        <TabsContent value="preview" className="pt-4"><PreviewPanel id={id} /></TabsContent>
      </Tabs>
    </>
  )
}

function Stats({ artifact: a }: { artifact: Artifact }) {
  const viewHint = a.max_views ? `${Math.max(0, a.max_views - a.views)} of ${a.max_views} left` : 'no limit'
  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <StatCard label="Views" value={a.views} hint={`${viewHint} · ${a.previews} link preview(s)`} progress={a.max_views ? (a.views / a.max_views) * 100 : undefined} />
      <StatCard label="Last viewed" value={a.last_viewed_at ? relativeTime(a.last_viewed_at) : 'never'} />
      <StatCard
        label="Expires"
        value={a.expires_at ? relativeTime(a.expires_at) : 'never'}
        hint={a.purge_at ? `content deleted ${formatDate(a.purge_at)}` : a.has_password ? 'password protected' : undefined}
      />
      <StatCard label="Size" value={formatBytes(a.size)} hint={`v${a.version} live · ${formatBytes(a.total_size)} across ${a.versions} version(s)`} />
    </div>
  )
}
