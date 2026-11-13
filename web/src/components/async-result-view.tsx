import type { ReactNode } from 'react'
import { AsyncResult } from 'effect/reactivity'
import { describeCause } from '@/api/errors'
import { EmptyState } from './empty-state'
import { PageSpinner } from './spinner'

type Props<A, E> = {
  result: AsyncResult.AsyncResult<A, E>
  children: (value: A, waiting: boolean) => ReactNode
  /** Rendered while there is no value yet (default: spinner). */
  loading?: ReactNode
}

/**
 * Renders an atom's AsyncResult: a spinner before the first value, the
 * failure in place (queries never toast), otherwise the value. A refresh
 * keeps showing the previous value with `waiting` set.
 */
export function AsyncResultView<A, E>({ result, children, loading }: Props<A, E>) {
  if (AsyncResult.isSuccess(result)) return <>{children(result.value, result.waiting)}</>
  if (AsyncResult.isFailure(result)) {
    const previous = result.previousSuccess
    if (previous._tag === 'Some') return <>{children(previous.value.value, false)}</>
    return <EmptyState title="Could not load this" hint={describeCause(result.cause)} />
  }
  return <>{loading ?? <PageSpinner />}</>
}
