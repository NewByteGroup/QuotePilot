// Same rule main.tsx uses to start the MSW contract mock.
export const MOCK_ENABLED: boolean =
  import.meta.env.VITE_API_MOCK === 'true' ||
  (import.meta.env.DEV && import.meta.env.VITE_API_MOCK !== 'false')

// The core-api request edge (auth, /me) is real today, so it can leave the mock on its own:
// VITE_CORE_API_MOCK=false sends those calls through the /api proxy while the endpoints
// core-api does not serve yet (web-api.yaml) stay mocked.
export const CORE_MOCK_ENABLED: boolean =
  MOCK_ENABLED && import.meta.env.VITE_CORE_API_MOCK !== 'false'
