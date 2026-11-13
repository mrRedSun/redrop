import { Atom } from 'effect/reactivity'
import { mutation, query } from '@/runtime/runtime'
import * as endpoints from './endpoints'
import { toCreateTokenRequest, type CreateTokenForm } from './schemas'

declare module 'effect/reactivity/Reactivity' {
  interface Keys {
    readonly tokens: never
  }
}

export const list = query(['tokens'], endpoints.list)

/**
 * The created secret lives only in this atom's AsyncResult. idleTTL 0: when
 * the "copy your token" dialog unmounts, the node (and the secret) is
 * dropped; the dialog also writes Atom.Reset on close. The tokens list
 * refetches a schema without a secret field.
 */
export const create = mutation(
  (values: typeof CreateTokenForm.Type) => endpoints.create(toCreateTokenRequest(values)),
  { invalidates: ['tokens'], failures: 'form' },
).pipe(Atom.setIdleTTL(0))

export const revoke = Atom.family((id: string) =>
  mutation((_: void) => endpoints.revoke(id), { invalidates: ['tokens'], success: 'Token revoked; the CLI using it stops working' }),
)
