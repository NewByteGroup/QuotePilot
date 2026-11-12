import { createOpenApiHttp } from 'openapi-msw'
import type { paths } from '@quotepilot/contracts/web-api'

import type { Schemas } from '@/api/client'
import { API_BASE_URL } from '@/api/client'

import {
  buildInboxQuotes,
  buildInboxSummary,
  countByStatus,
  ORGANIZATIONS,
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
import { buildQuoteDetail } from './quote-detail-fixtures'

const http = createOpenApiHttp<paths>({ baseUrl: API_BASE_URL })

function matchesSearch(quote: Schemas['InboxQuote'], search: string): boolean {
  const needle = search.trim().toLowerCase()
  if (!needle) return true
  const haystack = [String(quote.rfqNumber), quote.customerName, quote.subject]
  return haystack.some((value) => value.toLowerCase().includes(needle))
}

export function buildInboxQuotePage(
  all: Schemas['InboxQuote'][],
  now: Date,
  status: Schemas['QuoteStatus'] | undefined,
  search: string,
): Schemas['InboxQuotePage'] {
  const searched = all.filter((quote) => matchesSearch(quote, search))
  return {
    items: searched.filter((quote) => !status || quote.status === status),
    summary: buildInboxSummary(all, now),
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

function clearState() {
  const storage = globalThis.localStorage
  if (!storage) return
  Object.keys(storage)
    .filter((key) => key.startsWith(STORAGE_PREFIX))
    .forEach((key) => storage.removeItem(key))
}

const USER_LANGUAGES: Schemas['UserLanguage'][] = ['pt', 'en', 'es']

function buildSession(): Schemas['Session'] {
  return structuredClone(sessionFixture)
}

// Private data lives per organization (RLS in the real API); public intelligence is shared.
type OrganizationState = {
  pricingRules: Schemas['PricingRules']
  companyIdentity: Schemas['CompanyIdentity']
}

function isPrimary(organization: Schemas['SessionOrganization']): boolean {
  return organization.id === ORGANIZATIONS[0].id
}

function loadOrganizationState(organization: Schemas['SessionOrganization']): OrganizationState {
  // The first organization keeps edits saved before data became per-organization.
  const legacy = <T>(key: string, build: () => T) =>
    isPrimary(organization) ? loadState(key, build) : build()
  return {
    pricingRules: loadState(`pricing-rules.${organization.id}`, () =>
      legacy('pricing-rules', buildPricingRules),
    ),
    companyIdentity: loadState(`company-identity.${organization.id}`, () =>
      legacy('company-identity', () => buildCompanyIdentity(organization)),
    ),
  }
}

// A session saved by an older mock may lack fields added later: fill them from the fixture.
export function loadSession(): Schemas['Session'] {
  const stored = loadState<Partial<Schemas['Session']>>('session', buildSession)
  const fallback = buildSession()
  const organizations = fallback.organizations
  const organization =
    organizations.find((item) => item.id === stored.organization?.id) ?? fallback.organization
  return {
    user: { ...fallback.user, ...stored.user },
    organization,
    organizations,
  }
}

let session = loadSession()
const organizationStates = new Map<string, OrganizationState>()

function current(): OrganizationState {
  const organization = session.organization
  let state = organizationStates.get(organization.id)
  if (!state) {
    state = loadOrganizationState(organization)
    organizationStates.set(organization.id, state)
  }
  return state
}

function setPricingRules(next: Schemas['PricingRules']) {
  current().pricingRules = next
  saveState(`pricing-rules.${session.organization.id}`, next)
}

function setCompanyIdentity(next: Schemas['CompanyIdentity']) {
  current().companyIdentity = next
  saveState(`company-identity.${session.organization.id}`, next)
}

function setSession(next: Schemas['Session']) {
  session = next
  saveState('session', next)
}

// The second organization sees a smaller slice of the sample data.
function organizationQuotes(now: Date): Schemas['InboxQuote'][] {
  const all = buildInboxQuotes(now)
  return isPrimary(session.organization) ? all : all.filter((_, index) => index % 2 === 1)
}

function organizationNetRates(now: Date): Schemas['NetRateTablePage'] {
  const page = buildNetRateTablePage(now)
  return isPrimary(session.organization)
    ? page
    : { items: page.items.slice(0, 3), staleQuoteCount: 0 }
}

export function resetMockState() {
  clearState()
  organizationStates.clear()
  session = buildSession()
}

// Contract mock: lets web run without core-api (ADR-001, contracts first).
export const handlers = [
  http.get('/v1/session', ({ response }) => response(200).json(session)),
  http.post('/v1/session/organization', async ({ request, response }) => {
    const { organizationId } = await request.json()
    const organization = session.organizations.find((item) => item.id === organizationId)
    if (!organization) {
      return response(404).json({ code: 'not_found', message: 'Organization not found' })
    }
    setSession({ ...session, organization })
    return response(200).json(session)
  }),
  http.patch('/v1/me/preferences', async ({ request, response }) => {
    const { name, language } = await request.json()
    if (language !== undefined && !USER_LANGUAGES.includes(language)) {
      return response(400).json({ code: 'invalid_request', message: 'Unsupported language.' })
    }
    const trimmedName = name?.trim()
    if (trimmedName !== undefined && (trimmedName.length === 0 || trimmedName.length > 120)) {
      return response(400).json({ code: 'invalid_request', message: 'Name must have 1–120 chars.' })
    }
    setSession({
      ...session,
      user: {
        ...session.user,
        name: trimmedName ?? session.user.name,
        language: language ?? session.user.language,
      },
    })
    return response(200).json(session)
  }),
  http.get('/v1/inbox/quotes', ({ query, response }) => {
    const now = new Date()
    const status = query.get('status') as Schemas['QuoteStatus'] | null
    const page = buildInboxQuotePage(
      organizationQuotes(now),
      now,
      status ?? undefined,
      query.get('q') ?? '',
    )
    return response(200).json(page)
  }),
  http.get('/v1/quotes/{quoteId}', ({ params, response }) => {
    const quote = organizationQuotes(new Date()).find((item) => item.id === params.quoteId)
    if (!quote) {
      return response(404).json({ code: 'not_found', message: 'Quote not found' })
    }
    return response(200).json(buildQuoteDetail(quote, session.organization))
  }),
  http.get('/v1/rates/net-rate-tables', ({ response }) =>
    response(200).json(organizationNetRates(new Date())),
  ),
  http.get('/v1/pricing-rules', ({ response }) => response(200).json(current().pricingRules)),
  http.post('/v1/pricing-rules/route-margins', async ({ request, response }) => {
    const { routes } = await request.json()
    setPricingRules(addRouteMargins(current().pricingRules, routes))
    return response(200).json(current().pricingRules)
  }),
  http.patch(
    '/v1/pricing-rules/automations/{automationKey}',
    async ({ params, request, response }) => {
      const { enabled } = await request.json()
      const { pricingRules } = current()
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
  http.get('/v1/organization/identity', ({ response }) =>
    response(200).json(current().companyIdentity),
  ),
  http.patch('/v1/organization/identity', async ({ request, response }) => {
    const result = applyIdentityUpdate(current().companyIdentity, await request.json())
    if ('error' in result) {
      return response(400).json({ code: 'invalid_request', message: result.error })
    }
    setCompanyIdentity(result.identity)
    return response(200).json(current().companyIdentity)
  }),
  http.put('/v1/organization/identity/letterhead-logo', async ({ request, response }) => {
    const file = (await request.formData()).get('file')
    if (!file || typeof file === 'string') {
      return response(400).json({ code: 'invalid_request', message: 'file is required' })
    }
    if (!LETTERHEAD_LOGO_TYPES.includes(file.type) || file.size > LETTERHEAD_LOGO_MAX_BYTES) {
      return response(400).json({ code: 'invalid_request', message: 'PNG, JPEG or SVG up to 2 MB' })
    }
    setCompanyIdentity({
      ...current().companyIdentity,
      letterheadLogoUrl: await fileToDataUrl(file),
    })
    return response(200).json(current().companyIdentity)
  }),
  http.delete('/v1/organization/identity/letterhead-logo', ({ response }) => {
    setCompanyIdentity({ ...current().companyIdentity, letterheadLogoUrl: null })
    return response(200).json(current().companyIdentity)
  }),
  http.get('/v1/connectors/overview', ({ response }) =>
    response(200).json(buildConnectorOverview(new Date())),
  ),
  http.get('/v1/repairs/overview', ({ response }) =>
    response(200).json(buildRepairOverview(new Date())),
  ),
]
