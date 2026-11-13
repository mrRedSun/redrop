import { Cause, Effect, Layer } from 'effect'
import { BrowserHttpClient } from '@effect/platform-browser'
import { Atom, AtomRegistry, Reactivity, type AsyncResult } from 'effect/reactivity'
import { scheduleTask } from '@effect/atom-react'
import type { ApiError } from '@/api/errors'
import { RedropApi } from '@/api/redrop-api'
import { Notifier } from './notifier'

/**
 * The one runtime. Every query and mutation in the app runs here, so the
 * HttpClient, Reactivity and Notifier instances are shared, and tests swap
 * the whole world by swapping AppLayer.
 */
export const AppLayer = Layer.mergeAll(RedropApi.layer, Notifier.layer).pipe(Layer.provide(BrowserHttpClient.layerFetch))

export const appRuntime = Atom.runtime(AppLayer)

/** What feature effects may require. Notifier stays private to `mutation`. */
export type FeatureServices = RedropApi | Reactivity.Reactivity

/**
 * The registry lives outside React so router loaders, the session watcher
 * and components read the same atoms. Idle atoms survive 60 s, which is the
 * whole cache policy (no staleTime knobs).
 */
export const registry = AtomRegistry.make({ scheduleTask, defaultIdleTTL: 60_000 })

/** Await an async atom outside React (loaders, guards). Waits through refreshes, so it never returns a value known to be stale. */
export const readAtom = <A, E>(atom: Atom.Atom<AsyncResult.AsyncResult<A, E>>): Promise<A> =>
  Effect.runPromise(AtomRegistry.getResult(registry, atom, { suspendOnWaiting: true }))

/** Start loading an atom without waiting (route loaders call this so data and the lazy chunk load in parallel). */
export const prefetch = (atom: Atom.Atom<unknown>): void => void registry.get(atom)

/**
 * A server read. Refreshes whenever any of `keys` is invalidated; while
 * refreshing it keeps the previous value (AsyncResult `waiting`).
 */
export const query = <A, E>(keys: Reactivity.KeysInput, effect: Effect.Effect<A, E, FeatureServices>) =>
  appRuntime.atom(effect).pipe(Atom.withReactivity(keys))

export interface MutationOptions<Arg, A> {
  /** Feature keys whose queries refresh after success. */
  readonly invalidates: Reactivity.KeysInput
  /** Toast on success. */
  readonly success?: string | ((value: A, arg: Arg) => string)
  /**
   * "toast" (default): failures go to the central toast.
   * "form": the caller renders failures (useFormSubmit puts them on the form).
   * Defects always toast.
   */
  readonly failures?: 'toast' | 'form'
}

/**
 * A server write. Components get it as an atom (`useAtomSet(atom, { mode: 'promiseExit' })`)
 * and pass form values or ids, never request bodies.
 */
export const mutation = <Arg, A, E extends ApiError>(
  run: (arg: Arg) => Effect.Effect<A, E, FeatureServices>,
  options: MutationOptions<Arg, A>,
) =>
  appRuntime.fn<Arg>()(
    (arg) =>
      run(arg).pipe(
        Effect.tap((value) => {
          const message = typeof options.success === 'function' ? options.success(value, arg) : options.success
          return message ? Notifier.use((n) => n.success(message)) : Effect.void
        }),
        Effect.tapCause((cause) => {
          if (Cause.hasInterruptsOnly(cause)) return Effect.void // superseded or unmounted
          if (options.failures === 'form' && !Cause.hasDies(cause)) return Effect.void
          return Notifier.use((n) => n.failure(cause))
        }),
      ),
    { reactivityKeys: options.invalidates },
  ).pipe(Atom.setIdleTTL(0))
