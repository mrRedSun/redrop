import { useState } from 'react'
import { AsyncResult } from 'effect/reactivity'

/**
 * Filter lists are atom families keyed by the filter, so a new filter starts
 * at Initial. This keeps showing the last success (flagged `waiting`) until
 * the new one lands. Display-only: nothing is written back anywhere.
 */
export function useKeepPrevious<A, E>(result: AsyncResult.AsyncResult<A, E>): AsyncResult.AsyncResult<A, E> {
  const [last, setLast] = useState<AsyncResult.Success<A, E> | undefined>(undefined)
  // Derived state: remember the latest success (React's "adjust state during render" pattern).
  if (AsyncResult.isSuccess(result) && result !== last) setLast(result)
  if (AsyncResult.isInitial(result) && last) return AsyncResult.success(last.value, { waiting: true })
  return result
}
