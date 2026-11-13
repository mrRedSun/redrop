import { Atom } from 'effect/reactivity'
import { mutation, query } from '@/runtime/runtime'
import * as endpoints from './endpoints'
import { toUpdateRequest, type Artifact, type ArtifactFilter, type ArtifactSettings } from './schemas'

declare module 'effect/reactivity/Reactivity' {
  interface Keys {
    /** Every artifact read (lists, details, versions, files, logs). Feature-wide: record keys cannot target one id. */
    readonly artifacts: never
  }
}

// ---- reads ----

/** Keyed by the decoded filter; Effect's structural equality makes `{q, status}` a stable key. */
export const list = Atom.family((filter: ArtifactFilter) => query(['artifacts'], endpoints.list(filter)))
export const detail = Atom.family((id: string) => query(['artifacts'], endpoints.get(id)))
export const versions = Atom.family((id: string) => query(['artifacts'], endpoints.versions(id)))
export const views = Atom.family((id: string) => query(['artifacts'], endpoints.views(id)))

/** Key: `${id}@${version}`, version 0 = current. */
export const files = Atom.family((key: string) => {
  const [id, version] = key.split('@')
  return query(['artifacts'], endpoints.files(id, Number(version)))
})
export const filesKey = (id: string, version: number) => `${id}@${version}`

/** A fresh preview link each time the preview is opened; never kept around (it is a bearer link). */
export const preview = Atom.family((id: string) => query(['artifacts'], endpoints.preview(id)).pipe(Atom.setIdleTTL(0)))

// ---- writes (arguments are form values or plain choices, never request bodies) ----

export const saveSettings = Atom.family((id: string) =>
  mutation((input: { values: ArtifactSettings; kind: Artifact['kind'] }) => endpoints.update(id, toUpdateRequest(input.values, input.kind)), {
    invalidates: ['artifacts'],
    success: 'Settings saved',
    failures: 'form',
  }),
)

export const setRevoked = Atom.family((id: string) =>
  mutation((revoked: boolean) => endpoints.update(id, { revoked }), {
    invalidates: ['artifacts'],
    success: (a: Artifact) => (a.status === 'revoked' ? 'Link revoked' : 'Link restored'),
  }),
)

export const rotate = Atom.family((id: string) =>
  mutation((_: void) => endpoints.rotate(id), { invalidates: ['artifacts'], success: 'New link created; the old one stopped working' }),
)

export const remove = Atom.family((id: string) =>
  mutation((_: void) => endpoints.remove(id), { invalidates: ['artifacts'], success: 'Artifact deleted' }),
)

export const activateVersion = Atom.family((id: string) =>
  mutation((n: number) => endpoints.activateVersion(id, n), {
    invalidates: ['artifacts'],
    success: (_: Artifact, n: number) => `Version ${n} is now live`,
  }),
)

export const deleteVersion = Atom.family((id: string) =>
  mutation((n: number) => endpoints.deleteVersion(id, n), {
    invalidates: ['artifacts'],
    success: (_: void, n: number) => `Version ${n} deleted`,
  }),
)
