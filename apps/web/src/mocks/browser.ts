import { setupWorker } from 'msw/browser'

import { CORE_MOCK_ENABLED } from '@/lib/mock-flag'

import { authHandlers } from './auth-handlers'
import { handlers } from './handlers'

export const worker = setupWorker(...(CORE_MOCK_ENABLED ? authHandlers : []), ...handlers)
