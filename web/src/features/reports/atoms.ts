import { mutation } from '@/runtime/runtime'
import * as endpoints from './endpoints'
import { toReportRequest, type ReportValues } from './schemas'

/** Nothing is cached: the page shows its own confirmation. */
export const submit = mutation((v: ReportValues) => endpoints.submit(toReportRequest(v)), { invalidates: [], failures: 'form' })
