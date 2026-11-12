import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient, type Schemas } from './client'

export type InboxFilters = {
  status?: Schemas['QuoteStatus']
  q?: string
}

type ApiResult<T> = { data?: T; error?: Schemas['Error'] }

function unwrap<T>(operation: string, { data, error }: ApiResult<T>): T {
  if (error || data === undefined) {
    throw new Error(`${operation} failed: ${error?.code} ${error?.message}`)
  }
  return data
}

const PRICING_RULES_KEY = ['pricing-rules']
const COMPANY_IDENTITY_KEY = ['company-identity']

export function useSession() {
  return useQuery({
    queryKey: ['session'],
    queryFn: async () => unwrap('GET /v1/session', await apiClient.GET('/v1/session')),
  })
}

export function useInboxQuotes(filters: InboxFilters = {}) {
  return useQuery({
    queryKey: ['inbox-quotes', filters],
    queryFn: async () =>
      unwrap(
        'GET /v1/inbox/quotes',
        await apiClient.GET('/v1/inbox/quotes', {
          params: { query: { status: filters.status, q: filters.q || undefined } },
        }),
      ),
  })
}

export function useQuoteDetail(quoteId: string) {
  return useQuery({
    queryKey: ['quote-detail', quoteId],
    queryFn: async () =>
      unwrap(
        `GET /v1/quotes/${quoteId}`,
        await apiClient.GET('/v1/quotes/{quoteId}', { params: { path: { quoteId } } }),
      ),
  })
}

export function useNetRateTables() {
  return useQuery({
    queryKey: ['net-rate-tables'],
    queryFn: async () =>
      unwrap('GET /v1/rates/net-rate-tables', await apiClient.GET('/v1/rates/net-rate-tables')),
  })
}

export function usePricingRules() {
  return useQuery({
    queryKey: PRICING_RULES_KEY,
    queryFn: async () => unwrap('GET /v1/pricing-rules', await apiClient.GET('/v1/pricing-rules')),
  })
}

export function useAddRouteMargins() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (routes: Schemas['NewRouteMargin'][]) =>
      unwrap(
        'POST /v1/pricing-rules/route-margins',
        await apiClient.POST('/v1/pricing-rules/route-margins', { body: { routes } }),
      ),
    onSuccess: (rules) => queryClient.setQueryData(PRICING_RULES_KEY, rules),
  })
}

export function useUpdatePricingAutomation() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ key, enabled }: { key: string; enabled: boolean }) =>
      unwrap(
        `PATCH /v1/pricing-rules/automations/${key}`,
        await apiClient.PATCH('/v1/pricing-rules/automations/{automationKey}', {
          params: { path: { automationKey: key } },
          body: { enabled },
        }),
      ),
    onSuccess: (automation) =>
      queryClient.setQueryData<Schemas['PricingRules']>(PRICING_RULES_KEY, (rules) =>
        rules
          ? {
              ...rules,
              automations: rules.automations.map((item) =>
                item.key === automation.key ? automation : item,
              ),
            }
          : rules,
      ),
  })
}

export function useCompanyIdentity() {
  return useQuery({
    queryKey: COMPANY_IDENTITY_KEY,
    queryFn: async () =>
      unwrap('GET /v1/organization/identity', await apiClient.GET('/v1/organization/identity')),
  })
}

export function useConnectorOverview() {
  return useQuery({
    queryKey: ['connector-overview'],
    queryFn: async () =>
      unwrap('GET /v1/connectors/overview', await apiClient.GET('/v1/connectors/overview')),
  })
}

export function useRepairOverview() {
  return useQuery({
    queryKey: ['repair-overview'],
    queryFn: async () =>
      unwrap('GET /v1/repairs/overview', await apiClient.GET('/v1/repairs/overview')),
  })
}

export function useUpdateCompanyIdentity() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (patch: Schemas['UpdateCompanyIdentityRequest']) =>
      unwrap(
        'PATCH /v1/organization/identity',
        await apiClient.PATCH('/v1/organization/identity', { body: patch }),
      ),
    onSuccess: (identity) => queryClient.setQueryData(COMPANY_IDENTITY_KEY, identity),
  })
}

export function useUploadLetterheadLogo() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (file: File) =>
      unwrap(
        'PUT /v1/organization/identity/letterhead-logo',
        await apiClient.PUT('/v1/organization/identity/letterhead-logo', {
          // The generated type models `format: binary` as string; the serializer sends the File.
          body: { file: file as unknown as string },
          bodySerializer: (body) => {
            const form = new FormData()
            form.append('file', body.file)
            return form
          },
        }),
      ),
    onSuccess: (identity) => queryClient.setQueryData(COMPANY_IDENTITY_KEY, identity),
  })
}

export function useRemoveLetterheadLogo() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async () =>
      unwrap(
        'DELETE /v1/organization/identity/letterhead-logo',
        await apiClient.DELETE('/v1/organization/identity/letterhead-logo'),
      ),
    onSuccess: (identity) => queryClient.setQueryData(COMPANY_IDENTITY_KEY, identity),
  })
}
