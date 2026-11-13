import { Option } from 'effect'
import { AsyncResult } from 'effect/reactivity'
import { registry } from '@/runtime/runtime'
import { session } from '@/features/auth/atoms'
import { loginPath, paths } from './paths'

/**
 * The one place that reacts to "signed in -> signed out" (logout, or a 401
 * whose session re-check came back empty). A full reload to /login drops
 * every cached atom, so the next user can never see the previous user's data.
 */
export function watchSession(): () => void {
  let userId: string | null | undefined
  const unsubscribe = registry.subscribe(session, (result) => {
    if (!AsyncResult.isSuccess(result) || result.waiting) return
    const nextId = Option.isSome(result.value) ? result.value.value.user.id : null
    if (userId != null && nextId !== userId) {
      window.location.replace(nextId === null ? loginPath(location.pathname + location.search) : paths.artifacts)
    }
    userId = nextId
  }, { immediate: true })
  // Another tab may have signed out or switched accounts while this tab
  // was idle. Refetch on focus without polling or retaining identity in storage.
  const recheck = () => registry.refresh(session)
  window.addEventListener('focus', recheck)
  return () => {
    unsubscribe()
    window.removeEventListener('focus', recheck)
  }
}
