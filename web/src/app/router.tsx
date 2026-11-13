import { createBrowserRouter, redirect } from 'react-router'
import * as artifacts from '@/features/artifacts/atoms'
import { filterFromSearch } from '@/features/artifacts/schemas'
import * as auth from '@/features/auth/atoms'
import { prefetch } from '@/runtime/runtime'
import { PageSpinner } from '@/components/spinner'
import { requireAdmin, requireGuest, requireSession } from './guards'
import { AppLayout } from './layouts/app-layout'
import { PublicLayout } from './layouts/public-layout'
import { paths } from './paths'
import { NotFoundPage, RouteError } from './route-error'

/**
 * Every page is a lazy route module exporting `Component`. Loaders only guard
 * and prefetch (fire-and-forget, so the chunk and the data load in parallel);
 * pages read the same atoms through hooks, so there is one cache.
 */
export const router = createBrowserRouter([
  {
    errorElement: <RouteError />,
    HydrateFallback: PageSpinner,
    children: [
      {
        Component: PublicLayout,
        children: [
          { path: paths.login, loader: requireGuest, lazy: () => import('@/features/auth/pages/login-page') },
          { path: `${paths.setupPrefix}:token`, loader: () => (prefetch(auth.config), null), lazy: () => import('@/features/onboarding/pages/setup-page') },
          { path: `${paths.invitePrefix}:token`, lazy: () => import('@/features/onboarding/pages/invite-page') },
          { path: `${paths.resetPrefix}:token`, lazy: () => import('@/features/onboarding/pages/reset-page') },
        ],
      },
      {
        loader: requireSession,
        Component: AppLayout,
        children: [
          { index: true, loader: () => redirect(paths.artifacts) },
          {
            path: paths.artifacts,
            loader: async (args) => {
              await requireSession(args)
              prefetch(artifacts.list(filterFromSearch(new URL(args.request.url).searchParams)))
              return null
            },
            lazy: () => import('@/features/artifacts/pages/artifacts-page'),
          },
          {
            path: '/artifacts/:id',
            loader: async (args) => {
              await requireSession(args)
              prefetch(artifacts.detail(args.params.id ?? ''))
              return null
            },
            lazy: () => import('@/features/artifacts/pages/artifact-page'),
          },
          { path: paths.tokens, lazy: () => import('@/features/tokens/pages/tokens-page') },
          { path: paths.account, lazy: () => import('@/features/account/pages/account-page') },
          {
            path: paths.admin,
            loader: requireAdmin,
            lazy: () => import('@/features/admin/pages/admin-layout'),
            children: [
              { index: true, lazy: () => import('@/features/admin/pages/overview-page') },
              { path: paths.adminUsers, lazy: () => import('@/features/admin/pages/users-page') },
              { path: paths.adminInvites, lazy: () => import('@/features/admin/pages/invites-page') },
              { path: paths.adminArtifacts, lazy: () => import('@/features/admin/pages/artifacts-page') },
            ],
          },
          { path: '*', Component: NotFoundPage },
        ],
      },
    ],
  },
])
