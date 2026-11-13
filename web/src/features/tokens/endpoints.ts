import { Effect } from 'effect'
import { RedropApi, json } from '@/api/redrop-api'
import { CreateTokenRequest, CreatedToken, TokenList } from './schemas'

export const list = RedropApi.use((api) => api.call({ method: 'GET', path: ['tokens'], response: TokenList })).pipe(
  Effect.map((r) => r.items ?? []),
)

export const create = (req: typeof CreateTokenRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['tokens'], body: json(CreateTokenRequest, req), response: CreatedToken }))

export const revoke = (id: string) => RedropApi.use((api) => api.call({ method: 'DELETE', path: ['tokens', id] }))
