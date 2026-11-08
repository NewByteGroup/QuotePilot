import type { components, paths } from '@quotepilot/contracts/core-api'
import createClient from 'openapi-fetch'

export type Schemas = components['schemas']

export const API_BASE_URL: string = import.meta.env.VITE_API_BASE_URL ?? '/api'

// The only way web talks to core-api (ADR-001: generated client from packages/contracts).
export const apiClient = createClient<paths>({
  baseUrl: API_BASE_URL,
  // Resolve fetch per request so MSW (contract mock) can patch it after this module loads.
  fetch: (request) => globalThis.fetch(request),
})
