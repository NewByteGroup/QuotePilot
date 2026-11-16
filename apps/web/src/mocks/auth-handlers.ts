import { createOpenApiHttp } from 'openapi-msw'
import type { paths } from '@quotepilot/contracts/core-api'

import type { CoreSchemas } from '@/api/client'
import { API_BASE_URL } from '@/api/client'

import { ORGANIZATIONS, sessionFixture } from './fixtures'
import { MOCK_LOGIN_TOKEN } from './login-token'

const http = createOpenApiHttp<paths>({ baseUrl: API_BASE_URL })

const STORAGE_KEY = 'quotepilot.mock.signed-out'
const UNAUTHENTICATED = { error: 'unauthenticated' } as const

// The mock has no cookies: it starts signed in and only remembers an explicit logout.
function isSignedIn(): boolean {
  return globalThis.localStorage?.getItem(STORAGE_KEY) !== 'true'
}

function setSignedIn(signedIn: boolean) {
  if (signedIn) globalThis.localStorage?.removeItem(STORAGE_KEY)
  else globalThis.localStorage?.setItem(STORAGE_KEY, 'true')
}

export function resetMockAuth() {
  setSignedIn(true)
}

export const mockProfile: CoreSchemas['UserProfile'] = {
  id: sessionFixture.user.id,
  email: sessionFixture.user.email,
  full_name: sessionFixture.user.name,
}

export const mockMemberships: CoreSchemas['Membership'][] = ORGANIZATIONS.map(
  (organization, index) => ({
    organization_id: organization.id,
    name: organization.name,
    slug: organization.name.toLowerCase().replace(/[^a-z0-9]+/g, '-'),
    role: index === 0 ? 'OWNER' : 'PRICING_ANALYST',
  }),
)

// Mock of the core-api request edge (packages/contracts/openapi/core-api.yaml).
export const authHandlers = [
  http.post('/auth/login', async ({ request, response }) => {
    const { email } = await request.json()
    if (!email?.includes('@')) return response(400).json({ error: 'bad_request' })
    return response(200).json({ status: 'ok' })
  }),
  http.post('/auth/session', async ({ request, response }) => {
    const { token } = await request.json()
    if (token !== MOCK_LOGIN_TOKEN) return response(401).json(UNAUTHENTICATED)
    setSignedIn(true)
    return response(200).json({ profile: mockProfile, memberships: mockMemberships })
  }),
  http.post('/auth/refresh', ({ response }) =>
    isSignedIn() ? response(200).empty() : response(401).json(UNAUTHENTICATED),
  ),
  http.post('/auth/logout', ({ response }) => {
    setSignedIn(false)
    return response(204).empty()
  }),
  http.get('/me', ({ response }) =>
    isSignedIn()
      ? response(200).json({ ...mockProfile, memberships: mockMemberships })
      : response(401).json(UNAUTHENTICATED),
  ),
]
