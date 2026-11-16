import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'

import { mockMemberships, mockProfile } from '@/mocks/auth-handlers'
import { MOCK_LOGIN_TOKEN } from '@/mocks/login-token'
import { server } from '@/mocks/node'
import { renderApp } from '@/test/render'

const API = 'http://localhost/api'
const unauthenticated = () => HttpResponse.json({ error: 'unauthenticated' }, { status: 401 })

describe('Magic-link auth', () => {
  it('sends signed-out visitors to the login page', async () => {
    server.use(
      http.get(`${API}/me`, unauthenticated),
      http.post(`${API}/auth/refresh`, unauthenticated),
    )
    renderApp('/tarifas')
    expect(await screen.findByRole('heading', { name: 'Entrar' })).toBeInTheDocument()
  })

  it('rotates an expired access cookie once before giving up', async () => {
    let meCalls = 0
    server.use(
      http.get(`${API}/me`, () => {
        meCalls += 1
        return meCalls === 1
          ? unauthenticated()
          : HttpResponse.json({ ...mockProfile, memberships: mockMemberships })
      }),
    )
    renderApp('/')
    expect(await screen.findByRole('heading', { name: 'Inbox de cotações' })).toBeInTheDocument()
    expect(meCalls).toBe(2)
  })

  it('answers the same for any email and never reveals whether the account exists', async () => {
    server.use(
      http.get(`${API}/me`, unauthenticated),
      http.post(`${API}/auth/refresh`, unauthenticated),
    )
    renderApp('/login')
    await userEvent.type(await screen.findByLabelText('E-mail'), 'ninguem@exemplo.com')
    await userEvent.click(screen.getByRole('button', { name: 'Enviar link de acesso' }))
    expect(await screen.findByRole('status')).toHaveTextContent(
      'Se houver uma conta para ninguem@exemplo.com',
    )
  })

  it('exchanges the magic-link token and opens the app', async () => {
    renderApp(`/auth/exchange?token=${MOCK_LOGIN_TOKEN}`)
    expect(await screen.findByRole('heading', { name: 'Inbox de cotações' })).toBeInTheDocument()
  })

  it('rejects a used or unknown token and offers a new link', async () => {
    renderApp('/auth/exchange?token=stale')
    expect(
      await screen.findByRole('heading', { name: 'Link inválido ou expirado' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Pedir um novo link' })).toHaveAttribute(
      'href',
      '/login',
    )
  })

  it('logs out and returns to the login page', async () => {
    renderApp('/')
    await userEvent.click(await screen.findByRole('button', { name: 'Sair' }))
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Entrar' })).toBeInTheDocument())
    expect(localStorage.getItem('quotepilot.mock.signed-out')).toBe('true')
  })
})
