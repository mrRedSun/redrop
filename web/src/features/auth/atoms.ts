import { Effect } from 'effect'
import { Atom, Reactivity } from 'effect/reactivity'
import { mutation, query } from '@/runtime/runtime'
import * as endpoints from './endpoints'
import { toLoginRequest, type Credentials } from './schemas'

/** Kept alive: guards, layouts and the session watcher all read it. Value: Option<Me>. */
export const session = query(['session'], endpoints.getSession).pipe(Atom.keepAlive)

export interface LoginInput { readonly credentials: Credentials; readonly totp?: string }

/** Form failures (wrong password) render on the form; TotpRequired is a success value. */
export const login = mutation(
  ({ credentials, totp }: LoginInput) =>
    endpoints.login(toLoginRequest(credentials, totp)).pipe(
      Effect.tap((o) => (o._tag === 'SignedIn' ? Reactivity.invalidate(['session']) : Effect.void)),
    ),
  { invalidates: [], failures: 'form' },
)

/** The session watcher turns the resulting signed-out session into a full reload to /login. */
export const logout = mutation((_: void) => endpoints.logout, { invalidates: ['session'] })

declare module 'effect/reactivity/Reactivity' {
  interface Keys {
    /** Server login options; never changes while the page is open. */
    readonly authConfig: never
  }
}

/** Login options, server version, and public URLs. */
export const config = query(['authConfig'], endpoints.config).pipe(Atom.keepAlive)

export { ssoStartUrl } from './endpoints'
