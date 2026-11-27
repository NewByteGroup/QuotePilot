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
    const hits = { me: 0, session: 0 }
    const once401 = (key: keyof typeof hits, ok: object) => () => {
      hits[key] += 1
      return hits[key] === 1 ? unauthenticated() : HttpResponse.json(ok)
    }
    server.use(
      // Slow enough that both 401s land while this refresh is still in
      // flight — a second call afterwards means the flight was not shared.
      http.post(`${API}/auth/refresh`, async () => {
        refreshCalls += 1
        await new Promise((resolve) => setTimeout(resolve, 50))
        return new HttpResponse(null, { status: 204 })
      }),
      http.get(`${API}/me`, once401('me', {})),
      http.get(`${API}/v1/session`, once401('session', {})),
    )

    const [me, session] = await Promise.all([coreClient.GET('/me'), coreClient.GET('/v1/session')])

    expect(me.response.status).toBe(200)
    expect(session.response.status).toBe(200)
    expect(hits).toEqual({ me: 2, session: 2 })
    expect(refreshCalls).toBe(1)
  })
})
