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

// .env lives at the repo root (per .env.example), not in this package —
// envDir and loadEnv both point up so VITE_* overrides reach the dev
// server from the documented location.
const rootDir = fileURLToPath(new URL('../..', import.meta.url))

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, rootDir, '')
  const apiMock = resolveApiMockFlag(env)
  return {
    plugins: [react(), tailwindcss()],
    // Same-origin API: the browser calls /api/* and Vite forwards to core-api, so the
    // SameSite session cookies stay first-party (ADR-005 section 4). Only used when
    // the contract mock is off (VITE_API_MOCK=false).
    server: {
      proxy: {
        '/api': {
          target: env.CORE_API_URL || 'http://localhost:8080',
          changeOrigin: true,
          rewrite: (path) => path.replace(/^\/api/, ''),
        },
      },
    },
    envDir: rootDir,
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
