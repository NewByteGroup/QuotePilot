import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import App from './App'
import './index.css'
import { MOCK_ENABLED } from './lib/mock-flag'

// Production never mocks unless VITE_API_MOCK=true is set explicitly; `vite dev`
// mocks by default until core-api exists. Cloudflare Pages builds for non-main
// branches get VITE_API_MOCK=true injected by vite.config.ts.
async function startContractMock(): Promise<void> {
  if (!MOCK_ENABLED) return
  const { worker } = await import('./mocks/browser')
  await worker.start({ onUnhandledRequest: 'bypass' })
}

const queryClient = new QueryClient({
  defaultOptions: { queries: { refetchOnWindowFocus: false } },
})

startContractMock().then(() => {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </StrictMode>,
  )
})
