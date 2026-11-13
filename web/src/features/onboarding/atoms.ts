import { Effect } from 'effect'
import { Atom, Reactivity } from 'effect/reactivity'
import { mutation, query } from '@/runtime/runtime'
import * as endpoints from './endpoints'
import type { NewAccount, NewPasswordValues } from './schemas'

declare module 'effect/reactivity/Reactivity' {
  interface Keys {
    /** One-time link lookups (invite and reset info). */
    readonly onboarding: never
  }
}

/** Link lookups are not cached past the page: the token is a secret. */
export const inviteInfo = Atom.family((token: string) => query(['onboarding'], endpoints.inviteInfo(token)).pipe(Atom.setIdleTTL(0)))
export const resetInfo = Atom.family((token: string) => query(['onboarding'], endpoints.resetInfo(token)).pipe(Atom.setIdleTTL(0)))

/** Account-creating calls return the new session; re-reading `me` signs the browser in. */
const signedIn = Effect.tap(() => Reactivity.invalidate(['session']))

export const setup = Atom.family((token: string) =>
  mutation((v: NewAccount) => endpoints.setup({ token, username: v.username, password: v.password }).pipe(signedIn), {
    invalidates: [],
    success: 'Admin account created',
    failures: 'form',
  }),
)

export const acceptInvite = Atom.family((token: string) =>
  mutation((v: NewAccount) => endpoints.acceptInvite(token, { username: v.username, password: v.password }).pipe(signedIn), {
    invalidates: [],
    success: 'Welcome to redrop',
    failures: 'form',
  }),
)

export const resetPassword = Atom.family((token: string) =>
  mutation((v: NewPasswordValues) => endpoints.resetPassword(token, { password: v.password }), {
    invalidates: ['onboarding'],
    success: 'Password changed. Sign in with the new password.',
    failures: 'form',
  }),
)
