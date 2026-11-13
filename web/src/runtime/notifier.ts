import { Context, Effect, Layer } from 'effect'
import { toast } from 'sonner'
import { describeCause } from '@/api/errors'
import type { Cause } from 'effect'

/** User-visible notifications as an Effect service: sonner in the app, a recorder in tests. */
export class Notifier extends Context.Service<Notifier, {
  readonly success: (message: string) => Effect.Effect<void>
  readonly failure: (cause: Cause.Cause<unknown>) => Effect.Effect<void>
}>()('redrop/Notifier') {
  static readonly layer = Layer.succeed(Notifier, Notifier.of({
    success: (message) => Effect.sync(() => void toast.success(message)),
    failure: (cause) => Effect.sync(() => void toast.error(describeCause(cause))),
  }))
}
