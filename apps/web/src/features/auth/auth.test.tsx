import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'

import { mockMemberships, mockProfile } from '@/mocks/auth-handlers'
import { MOCK_LOGIN_TOKEN, MOCK_PASSWORD, MOCK_RESET_TOKEN } from '@/mocks/demo-credentials'
import { server } from '@/mocks/node'
import { renderApp } from '@/test/render'

const API = 'http://localhost/api'
const unauthenticated = () => HttpResponse.json({ error: 'unauthenticated' }, { status: 401 })

describe('Auth', () => {
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

  it('signs in with email and password', async () => {
    localStorage.setItem('quotepilot.mock.signed-out', 'true')
    renderApp('/login')
    await userEvent.type(await screen.findByLabelText('E-mail'), 'marcelo@atlantico.com.br')
    await userEvent.type(screen.getByLabelText('Senha'), MOCK_PASSWORD)
    await userEvent.click(screen.getByRole('button', { name: 'Entrar' }))
    expect(await screen.findByRole('heading', { name: 'Inbox de cotações' })).toBeInTheDocument()
  })

  it('rejects a wrong password without saying which field is wrong', async () => {
    server.use(
      http.get(`${API}/me`, unauthenticated),
      http.post(`${API}/auth/refresh`, unauthenticated),
    )
    renderApp('/login')
    await userEvent.type(await screen.findByLabelText('E-mail'), 'marcelo@atlantico.com.br')
    await userEvent.type(screen.getByLabelText('Senha'), 'errada')
    await userEvent.click(screen.getByRole('button', { name: 'Entrar' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('E-mail ou senha incorretos.')
  })

  it('answers the same password-reset message for any email', async () => {
    server.use(
      http.get(`${API}/me`, unauthenticated),
      http.post(`${API}/auth/refresh`, unauthenticated),
    )
    renderApp('/login')
    await userEvent.click(await screen.findByRole('link', { name: 'Esqueci minha senha' }))
    await userEvent.type(await screen.findByLabelText('E-mail'), 'ninguem@exemplo.com')
    await userEvent.click(screen.getByRole('button', { name: 'Enviar link de recuperação' }))
    expect(await screen.findByRole('status')).toHaveTextContent(
      'Se houver uma conta para ninguem@exemplo.com',
    )
  })

  it('sets a new password from the reset link', async () => {
    renderApp(`/auth/set-password?token=${MOCK_RESET_TOKEN}`)
    await userEvent.type(await screen.findByLabelText('Nova senha'), 'nova-senha-123')
    await userEvent.type(screen.getByLabelText('Confirmar senha'), 'nova-senha-123')
    await userEvent.click(screen.getByRole('button', { name: 'Salvar nova senha' }))
    expect(await screen.findByRole('heading', { name: 'Senha alterada' })).toBeInTheDocument()
  })

  it('rejects a used or unknown reset token', async () => {
    renderApp('/auth/set-password?token=stale')
    await userEvent.type(await screen.findByLabelText('Nova senha'), 'nova-senha-123')
    await userEvent.type(screen.getByLabelText('Confirmar senha'), 'nova-senha-123')
    await userEvent.click(screen.getByRole('button', { name: 'Salvar nova senha' }))
    expect(
      await screen.findByRole('heading', { name: 'Link inválido ou expirado' }),
    ).toBeInTheDocument()
  })

  it('exchanges the magic-link token and opens the app', async () => {
    renderApp(`/auth/exchange?token=${MOCK_LOGIN_TOKEN}`)
    expect(await screen.findByRole('heading', { name: 'Inbox de cotações' })).toBeInTheDocument()
  })

  it('rejects a used or unknown magic-link token', async () => {
    renderApp('/auth/exchange?token=stale')
    expect(
      await screen.findByRole('heading', { name: 'Link inválido ou expirado' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Voltar para o login' })).toHaveAttribute(
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
