import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'

import { coreClient } from '@/api/client'
import { server } from '@/mocks/node'

const API = 'http://localhost/api'
const unauthenticated = () => HttpResponse.json({ error: 'unauthenticated' }, { status: 401 })

describe('coreClient access-rotation middleware', () => {
  it('resends the request body on the post-refresh retry', async () => {
    const bodies: string[] = []
    server.use(
      http.post(`${API}/auth/refresh`, () => new HttpResponse(null, { status: 204 })),
      http.post(`${API}/v1/session/organization`, async ({ request }) => {
        bodies.push(await request.text())
        return bodies.length === 1
          ? unauthenticated()
          : HttpResponse.json({ user: {}, active_organization: {} })
      }),
    )

    const { response, error } = await coreClient.POST('/v1/session/organization', {
      body: { organizationId: '3f9a5b6e-1c2d-4e5f-8a9b-0c1d2e3f4a5b' },
    })

    expect(error).toBeUndefined()
    expect(response.status).toBe(200)
    expect(bodies).toHaveLength(2)
    expect(bodies[1]).toBe(bodies[0])
    expect(bodies[1]).toContain('3f9a5b6e')
  })

  it('shares one refresh across concurrent 401s', async () => {
    let refreshCalls = 0
    const meCalls: number[] = []
    server.use(
      http.post(`${API}/auth/refresh`, () => {
        refreshCalls += 1
        return new HttpResponse(null, { status: 204 })
      }),
      http.get(`${API}/me`, () => {
        meCalls.push(1)
        return meCalls.length <= 1
          ? unauthenticated()
          : HttpResponse.json({ user: {}, memberships: [] })
      }),
      http.get(`${API}/v1/session`, () => {
        meCalls.push(1)
        return meCalls.length <= 2
          ? unauthenticated()
          : HttpResponse.json({ user: {}, memberships: [] })
      }),
    )

    const [me, session] = await Promise.all([coreClient.GET('/me'), coreClient.GET('/v1/session')])

    expect(me.response.status).toBe(200)
    expect(session.response.status).toBe(200)
    expect(refreshCalls).toBe(1)
  })
})
