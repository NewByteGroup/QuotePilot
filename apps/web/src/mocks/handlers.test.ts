import { describe, expect, it } from 'vitest'

import { ORGANIZATIONS, sessionFixture } from './fixtures'
import { loadSession } from './handlers'

describe('mock session persistence', () => {
  it('fills a session saved by an older mock with the fields it lacks', () => {
    localStorage.setItem(
      'quotepilot.mock.session',
      JSON.stringify({
        user: { id: sessionFixture.user.id, name: 'Marcelo C.', role: 'admin', language: 'en' },
        organization: { id: ORGANIZATIONS[1].id, name: 'Old name' },
      }),
    )

    const session = loadSession()

    expect(session.user).toMatchObject({ name: 'Marcelo C.', language: 'en' })
    expect(session.user.email).toBe(sessionFixture.user.email)
    expect(session.organization).toEqual(ORGANIZATIONS[1])
    expect(session.organizations).toEqual(ORGANIZATIONS)
  })

  it('falls back to the first organization when the saved one no longer exists', () => {
    localStorage.setItem(
      'quotepilot.mock.session',
      JSON.stringify({ organization: { id: 'missing' } }),
    )
    expect(loadSession().organization).toEqual(ORGANIZATIONS[0])
  })
})
