import { setupServer } from 'msw/node'

import { authHandlers } from './auth-handlers'
import { handlers } from './handlers'

export const server = setupServer(...authHandlers, ...handlers)
