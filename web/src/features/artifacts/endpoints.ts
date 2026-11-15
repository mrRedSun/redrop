import { Effect } from 'effect'
import { RedropApi, json } from '@/api/redrop-api'
import {
  Artifact,
  ArtifactList,
  CommentList,
  Comment,
  FileList,
  PreviewLink,
  UpdateArtifactRequest,
  VersionList,
  ViewList,
  ResolveCommentRequest,
  type ArtifactFilter,
  type FeedbackStatus,
} from './schemas'

// Typed endpoint functions over RedropApi; the only artifacts module that knows paths.

export const list = (f: ArtifactFilter) =>
  RedropApi.use((api) =>
    api.call({ method: 'GET', path: ['artifacts'], query: { q: f.q, status: f.status }, response: ArtifactList }),
  ).pipe(Effect.map((r) => r.items ?? []))

export const get = (id: string) => RedropApi.use((api) => api.call({ method: 'GET', path: ['artifacts', id], response: Artifact }))

export const update = (id: string, req: UpdateArtifactRequest) =>
  RedropApi.use((api) =>
    api.call({ method: 'PATCH', path: ['artifacts', id], body: json(UpdateArtifactRequest, req), response: Artifact }),
  )

export const rotate = (id: string) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['artifacts', id, 'rotate'], response: Artifact }))

export const remove = (id: string) => RedropApi.use((api) => api.call({ method: 'DELETE', path: ['artifacts', id] }))

/** Short-lived owner link that bypasses gates and does not count as a view. */
export const preview = (id: string) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['artifacts', id, 'preview'], response: PreviewLink }))

export const versions = (id: string) =>
  RedropApi.use((api) => api.call({ method: 'GET', path: ['artifacts', id, 'versions'], response: VersionList })).pipe(
    Effect.map((r) => r.items ?? []),
  )

export const activateVersion = (id: string, n: number) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['artifacts', id, 'versions', n, 'activate'], response: Artifact }))

export const deleteVersion = (id: string, n: number) =>
  RedropApi.use((api) => api.call({ method: 'DELETE', path: ['artifacts', id, 'versions', n] }))

/** Files of a version; `version` 0 means the current one. */
export const files = (id: string, version: number) =>
  RedropApi.use((api) =>
    api.call({
      method: 'GET',
      path: ['artifacts', id, 'files'],
      query: { version: version > 0 ? String(version) : undefined },
      response: FileList,
    }),
  ).pipe(Effect.map((r) => ({ version: r.version, items: r.items ?? [] })))

export const views = (id: string) =>
  RedropApi.use((api) => api.call({ method: 'GET', path: ['artifacts', id, 'views'], response: ViewList })).pipe(
    Effect.map((r) => r.items ?? []),
  )

/** Viewer comments, oldest first. Untrusted text. */
export const feedback = (id: string, status: FeedbackStatus) =>
  RedropApi.use((api) =>
    api.call({ method: 'GET', path: ['artifacts', id, 'feedback'], query: { status }, response: CommentList }),
  ).pipe(Effect.map((r) => r.items ?? []))

export const resolveComment = (id: string, commentId: string, resolved: boolean) =>
  RedropApi.use((api) =>
    api.call({
      method: 'PATCH',
      path: ['artifacts', id, 'feedback', commentId],
      body: json(ResolveCommentRequest, { resolved }),
      response: Comment,
    }),
  )

export const deleteComment = (id: string, commentId: string) =>
  RedropApi.use((api) => api.call({ method: 'DELETE', path: ['artifacts', id, 'feedback', commentId] }))
