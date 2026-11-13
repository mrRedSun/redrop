import { Atom } from 'effect/reactivity'
import { mutation, query } from '@/runtime/runtime'
import * as endpoints from './endpoints'
import { toChangePasswordRequest, toUpdateMeRequest, type PasswordValues, type ProfileValues, type TotpCodeValues } from './schemas'

declare module 'effect/reactivity/Reactivity' {
  interface Keys {
    /** Linked single sign-on identities. */
    readonly identities: never
  }
}

export const identities = query(['identities'], endpoints.identities)

/** Profile, password, and authenticator changes refresh the session (`me`). */
export const updateProfile = mutation((v: ProfileValues) => endpoints.updateProfile(toUpdateMeRequest(v)), {
  invalidates: ['session'],
  success: 'Profile saved',
  failures: 'form',
})

export const changePassword = mutation((v: PasswordValues) => endpoints.changePassword(toChangePasswordRequest(v)), {
  invalidates: [],
  success: 'Password changed; other sessions were signed out',
  failures: 'form',
})

/** The enrollment secret lives only while the enrollment card is mounted. */
export const beginTotp = mutation((_: void) => endpoints.beginTotp, { invalidates: [] }).pipe(Atom.setIdleTTL(0))

export const enableTotp = mutation((v: TotpCodeValues) => endpoints.enableTotp(v.code), {
  invalidates: ['session'],
  success: 'Two-step verification is on',
  failures: 'form',
})

export const disableTotp = mutation((v: TotpCodeValues) => endpoints.disableTotp(v.code), {
  invalidates: ['session'],
  success: 'Two-step verification is off',
  failures: 'form',
})

export const unlinkIdentity = Atom.family((id: string) =>
  mutation((_: void) => endpoints.unlinkIdentity(id), { invalidates: ['identities'], success: 'Identity unlinked' }),
)
