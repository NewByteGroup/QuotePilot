import { useQuery } from '@tanstack/react-query'

import { apiClient, type Schemas } from './client'

export type InboxFilters = {
  status?: Schemas['QuoteStatus']
  q?: string
}

async function fetchSession(): Promise<Schemas['Session']> {
  const { data, error } = await apiClient.GET('/v1/session')
  if (error) throw new Error(`GET /v1/session failed: ${error.code} ${error.message}`)
  return data
}

async function fetchInboxQuotes(filters: InboxFilters): Promise<Schemas['InboxQuotePage']> {
  const { data, error } = await apiClient.GET('/v1/inbox/quotes', {
    params: { query: { status: filters.status, q: filters.q || undefined } },
  })
  if (error) throw new Error(`GET /v1/inbox/quotes failed: ${error.code} ${error.message}`)
  return data
}

async function fetchQuoteDetail(quoteId: string): Promise<Schemas['QuoteDetail']> {
  const { data, error } = await apiClient.GET('/v1/quotes/{quoteId}', {
    params: { path: { quoteId } },
  })
  if (error) throw new Error(`GET /v1/quotes/${quoteId} failed: ${error.code} ${error.message}`)
  return data
}

export function useQuoteDetail(quoteId: string) {
  return useQuery({ queryKey: ['quote-detail', quoteId], queryFn: () => fetchQuoteDetail(quoteId) })
}

export function useSession() {
  return useQuery({ queryKey: ['session'], queryFn: fetchSession })
}

export function useInboxQuotes(filters: InboxFilters = {}) {
  return useQuery({
    queryKey: ['inbox-quotes', filters],
    queryFn: () => fetchInboxQuotes(filters),
  })
}
