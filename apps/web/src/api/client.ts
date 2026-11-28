import type {
  components as coreComponents,
  paths as corePaths,
} from '@quotepilot/contracts/core-api'
import type { components as webComponents, paths as webPaths } from '@quotepilot/contracts/web-api'
import createClient from 'openapi-fetch'

export type Schemas = webComponents['schemas']
export type CoreSchemas = coreComponents['schemas']

// One runtime client for the whole request edge. core-api.yaml wins where a
// path exists in both contracts; web-api.yaml contributes only the paths the
// backend has not folded in yet, so it remains the contract-mock source
// without becoming a second HTTP client.
type AppPaths = corePaths & Omit<webPaths, keyof corePaths>

export const API_BASE_URL: string = import.meta.env.VITE_API_BASE_URL ?? '/api'

// Resolve fetch per request so MSW (contract mock) can patch it after this module loads.
const fetchPerRequest = (request: Request) => globalThis.fetch(request)

// The only way web talks to core-api (ADR-001: generated client from packages/contracts).
// Sessions travel in the HttpOnly qp_access/qp_refresh cookies (ADR-005), so every call
// sends credentials; web and API share one origin through the /api proxy, which keeps
// the SameSite cookies first-party.
export const coreClient = createClient<AppPaths>({
  baseUrl: API_BASE_URL,
  credentials: 'include',
  fetch: fetchPerRequest,
})

// Single-flight access rotation: the first 401 outside /auth/* posts
// /auth/refresh once — concurrent 401s share it — and every caller retries
// its own request afterwards. Without this, the 15-minute qp_access TTL
// kills any protected read that is not loadMe's hand-rolled refresh.
let refreshing: Promise<boolean> | null = null

// A fetch consumes the Request's body stream, so the retry cannot resend the
// same object — a POST would die with TypeError right after the refresh it
// needed. onRequest stashes an undisturbed clone per call for the retry.
const retriable = new WeakMap<Request, Request>()

coreClient.use({
  onRequest({ request }) {
    retriable.set(request, request.clone())
    return request
  },
  async onResponse({ request, response }) {
    if (response.status !== 401) return response
    if (request.url.includes('/auth/')) return response
    refreshing ??= coreClient
      .POST('/auth/refresh')
      .then(({ response: r }) => r.ok)
      .finally(() => {
        refreshing = null
      })
    if (!(await refreshing)) return response
    const retry = retriable.get(request) ?? request
    retriable.delete(request)
    return fetchPerRequest(retry)
  },
})
