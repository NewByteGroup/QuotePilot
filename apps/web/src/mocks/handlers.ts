import { createOpenApiHttp } from 'openapi-msw'
import type { paths } from '@quotepilot/contracts/core-api'

import type { Schemas } from '@/api/client'
import { API_BASE_URL } from '@/api/client'

import {
  buildInboxQuotes,
  buildInboxSummary,
  buildQuoteDetail,
  countByStatus,
  sessionFixture,
} from './fixtures'

const http = createOpenApiHttp<paths>({ baseUrl: API_BASE_URL })

function matchesSearch(quote: Schemas['InboxQuote'], search: string): boolean {
  const needle = search.trim().toLowerCase()
  if (!needle) return true
  const haystack = [String(quote.rfqNumber), quote.customerName, quote.subject]
  return haystack.some((value) => value.toLowerCase().includes(needle))
}

export function buildInboxQuotePage(
  now: Date,
  status: Schemas['QuoteStatus'] | undefined,
  search: string,
): Schemas['InboxQuotePage'] {
  const all = buildInboxQuotes(now)
  const searched = all.filter((quote) => matchesSearch(quote, search))
  return {
    items: searched.filter((quote) => !status || quote.status === status),
    summary: buildInboxSummary(all),
    statusCounts: countByStatus(searched),
    refreshedAt: new Date(now.getTime() - 2 * 60_000).toISOString(),
  }
}

// Contract mock: lets web run without core-api (ADR-001, contracts first).
export const handlers = [
  http.get('/v1/session', ({ response }) => response(200).json(sessionFixture)),
  http.get('/v1/inbox/quotes', ({ query, response }) => {
    const status = query.get('status') as Schemas['QuoteStatus'] | null
    const page = buildInboxQuotePage(new Date(), status ?? undefined, query.get('q') ?? '')
    return response(200).json(page)
  }),
  http.get('/v1/quotes/{quoteId}', ({ params, response }) => {
    const quote = buildInboxQuotes(new Date()).find((item) => item.id === params.quoteId)
    if (!quote) {
      return response(404).json({ code: 'not_found', message: 'Quote not found' })
    }
    return response(200).json(buildQuoteDetail(quote))
  }),
]
