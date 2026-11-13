import { MutationCache, QueryCache, QueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { errorMessage, isUnauthorized } from '@/api/http'
import { authKeys } from '@/features/auth/queries'

declare module '@tanstack/react-query' {
  interface Register {
    mutationMeta: {
      /** Toast shown when the mutation succeeds. */
      successMessage?: string
      /** Set when the calling component renders the error itself. */
      silentError?: boolean
    }
  }
}

/**
 * Cross-cutting behavior lives here so feature hooks stay declarative:
 * mutation error/success toasts, and treating any 401 as a signed-out session.
 */
export function createQueryClient(): QueryClient {
  const client: QueryClient = new QueryClient({
    queryCache: new QueryCache({
      onError: (error, query) => {
        if (isUnauthorized(error) && query.queryKey[0] !== authKeys.me[0]) {
          client.setQueryData(authKeys.me, null)
        }
      },
    }),
    mutationCache: new MutationCache({
      onError: (error, _vars, _ctx, mutation) => {
        if (isUnauthorized(error)) client.setQueryData(authKeys.me, null)
        if (!mutation.meta?.silentError) toast.error(errorMessage(error))
      },
      onSuccess: (_data, _vars, _ctx, mutation) => {
        if (mutation.meta?.successMessage) toast.success(mutation.meta.successMessage)
      },
    }),
    defaultOptions: {
      queries: {
        staleTime: 15_000,
        refetchOnWindowFocus: true,
        retry: (count, error) => !isUnauthorized(error) && count < 2,
      },
    },
  })
  return client
}
