import { createOpenApiHttp } from 'openapi-msw'
import type { paths } from '@quotepilot/contracts/web-api'

import type { Schemas } from '@/api/client'
import { API_BASE_URL } from '@/api/client'

import {
  buildInboxQuotes,
  buildInboxSummary,
  buildQuoteDetail,
  countByStatus,
  sessionFixture,
} from './fixtures'
import {
  addRouteMargins,
  applyIdentityUpdate,
  fileToDataUrl,
  LETTERHEAD_LOGO_MAX_BYTES,
  LETTERHEAD_LOGO_TYPES,
  buildCompanyIdentity,
  buildConnectorOverview,
  buildNetRateTablePage,
  buildPricingRules,
  buildRepairOverview,
} from './platform-fixtures'

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

const STORAGE_PREFIX = 'quotepilot.mock.'

// Mock edits are kept in the browser so they survive a reload, like the real API would.
function loadState<T>(key: string, fallback: () => T): T {
  try {
    const stored = globalThis.localStorage?.getItem(STORAGE_PREFIX + key)
    return stored ? (JSON.parse(stored) as T) : fallback()
  } catch {
    return fallback()
  }
}

function saveState(key: string, value: unknown) {
  try {
    globalThis.localStorage?.setItem(STORAGE_PREFIX + key, JSON.stringify(value))
  } catch {
    // Storage full or unavailable: keep the change in memory only.
  }
}

let pricingRules = loadState('pricing-rules', buildPricingRules)
let companyIdentity = loadState('company-identity', buildCompanyIdentity)

function setPricingRules(next: Schemas['PricingRules']) {
  pricingRules = next
  saveState('pricing-rules', next)
}

function setCompanyIdentity(next: Schemas['CompanyIdentity']) {
  companyIdentity = next
  saveState('company-identity', next)
}

export function resetMockState() {
  globalThis.localStorage?.removeItem(STORAGE_PREFIX + 'pricing-rules')
  globalThis.localStorage?.removeItem(STORAGE_PREFIX + 'company-identity')
  pricingRules = buildPricingRules()
  companyIdentity = buildCompanyIdentity()
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
  http.get('/v1/rates/net-rate-tables', ({ response }) =>
    response(200).json(buildNetRateTablePage(new Date())),
  ),
  http.get('/v1/pricing-rules', ({ response }) => response(200).json(pricingRules)),
  http.post('/v1/pricing-rules/route-margins', async ({ request, response }) => {
    const { routes } = await request.json()
    setPricingRules(addRouteMargins(pricingRules, routes))
    return response(200).json(pricingRules)
  }),
  http.patch(
    '/v1/pricing-rules/automations/{automationKey}',
    async ({ params, request, response }) => {
      const { enabled } = await request.json()
      const automation = pricingRules.automations.find((item) => item.key === params.automationKey)
      if (!automation) {
        return response(404).json({ code: 'not_found', message: 'Automation not found' })
      }
      const updated = { ...automation, enabled }
      setPricingRules({
        ...pricingRules,
        automations: pricingRules.automations.map((item) =>
          item.key === updated.key ? updated : item,
        ),
      })
      return response(200).json(updated)
    },
  ),
  http.get('/v1/organization/identity', ({ response }) => response(200).json(companyIdentity)),
  http.patch('/v1/organization/identity', async ({ request, response }) => {
    const result = applyIdentityUpdate(companyIdentity, await request.json())
    if ('error' in result) {
      return response(400).json({ code: 'invalid_request', message: result.error })
    }
    setCompanyIdentity(result.identity)
    return response(200).json(companyIdentity)
  }),
  http.put('/v1/organization/identity/letterhead-logo', async ({ request, response }) => {
    const file = (await request.formData()).get('file')
    if (!file || typeof file === 'string') {
      return response(400).json({ code: 'invalid_request', message: 'file is required' })
    }
    if (!LETTERHEAD_LOGO_TYPES.includes(file.type) || file.size > LETTERHEAD_LOGO_MAX_BYTES) {
      return response(400).json({ code: 'invalid_request', message: 'PNG, JPEG or SVG up to 2 MB' })
    }
    setCompanyIdentity({ ...companyIdentity, letterheadLogoUrl: await fileToDataUrl(file) })
    return response(200).json(companyIdentity)
  }),
  http.delete('/v1/organization/identity/letterhead-logo', ({ response }) => {
    setCompanyIdentity({ ...companyIdentity, letterheadLogoUrl: null })
    return response(200).json(companyIdentity)
  }),
  http.get('/v1/connectors/overview', ({ response }) =>
    response(200).json(buildConnectorOverview(new Date())),
  ),
  http.get('/v1/repairs/overview', ({ response }) =>
    response(200).json(buildRepairOverview(new Date())),
  ),
]
