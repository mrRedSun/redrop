import { Option } from 'effect'
import { data, redirect, type LoaderFunctionArgs } from 'react-router'
import { readAtom } from '@/runtime/runtime'
import { session } from '@/features/auth/atoms'
import { loginPath, safeNext } from './paths'

/**
 * Guards are loaders: they run before any protected chunk renders, read the
 * `session` atom from the registry (no React), and either redirect or pass
 * the session down. A network failure rejects and lands in the route error element.
 */
export async function requireSession({ request }: LoaderFunctionArgs) {
  const me = await readAtom(session)
  if (Option.isNone(me)) {
    const url = new URL(request.url)
    throw redirect(loginPath(url.pathname + url.search))
  }
  return me.value
}

export async function requireGuest({ request }: LoaderFunctionArgs) {
  if (Option.isSome(await readAtom(session))) throw redirect(safeNext(new URL(request.url).searchParams.get('next')))
  return null
}

export async function requireAdmin(args: LoaderFunctionArgs) {
  const me = await requireSession(args)
  if (!me.can_admin) {
    throw data(
      me.user.role === 'admin' ? 'Admin pages require signing in with single sign-on.' : 'Only administrators can open this page.',
      { status: 403 },
    )
  }
  return me
}
