import { RedropApi, json } from '@/api/redrop-api'
import { ReportRequest } from './schemas'

/** Files a report. The answer is the same whether or not the link exists. */
export const submit = (req: typeof ReportRequest.Type) =>
  RedropApi.use((api) => api.call({ method: 'POST', path: ['reports'], body: json(ReportRequest, req) }))
