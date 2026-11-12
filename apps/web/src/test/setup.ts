import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterAll, afterEach, beforeAll } from 'vitest'

import i18n from '@/i18n'
import { resetMockState } from '@/mocks/handlers'
import { server } from '@/mocks/node'

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  cleanup()
  server.resetHandlers()
  resetMockState()
  void i18n.changeLanguage('pt')
})
afterAll(() => server.close())
