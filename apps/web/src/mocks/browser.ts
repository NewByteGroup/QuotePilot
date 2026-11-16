import { setupWorker } from 'msw/browser'

import { authHandlers } from './auth-handlers'
import { handlers } from './handlers'

export const worker = setupWorker(...authHandlers, ...handlers)
