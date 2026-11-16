// Same rule main.tsx uses to start the MSW contract mock.
export const MOCK_ENABLED: boolean =
  import.meta.env.VITE_API_MOCK === 'true' ||
  (import.meta.env.DEV && import.meta.env.VITE_API_MOCK !== 'false')
