/// <reference types="vitest/config" />
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'
import { defineConfig, loadEnv } from 'vite'

// Cloudflare Pages injects CF_PAGES_BRANCH at build time: the production site
// builds from 'main' and must not serve the MSW contract mock unless
// VITE_API_MOCK is set explicitly; every other branch deploy gets it on.
// Returns undefined for local builds so main.tsx's own defaults apply.
function resolveApiMockFlag(env: Record<string, string>): string | undefined {
  if (env.VITE_API_MOCK !== undefined) return env.VITE_API_MOCK
  if (env.CF_PAGES_BRANCH === undefined) return undefined
  return env.CF_PAGES_BRANCH === 'main' ? 'false' : 'true'
}

export default defineConfig(({ mode }) => {
  const apiMock = resolveApiMockFlag(loadEnv(mode, process.cwd(), ''))
  return {
    plugins: [react(), tailwindcss()],
    resolve: {
      alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
    },
    ...(apiMock === undefined
      ? {}
      : { define: { 'import.meta.env.VITE_API_MOCK': JSON.stringify(apiMock) } }),
    test: {
      environment: 'jsdom',
      setupFiles: ['./src/test/setup.ts'],
      env: { VITE_API_BASE_URL: 'http://localhost/api' },
    },
  }
})
