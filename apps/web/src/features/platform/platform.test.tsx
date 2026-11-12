import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderApp } from '@/test/render'

describe('Platform screens', () => {
  it('shows connector health and the live repair', async () => {
    renderApp('/conectores')

    const tcp = await screen.findByRole('article', { name: 'TCP Paranaguá · landside' })
    expect(within(tcp).getByText('Devin acionado')).toBeInTheDocument()
    expect(screen.getByText('1 fonte degradada (TCP)')).toBeInTheDocument()
    expect(screen.getByText('PR #84 merged após revisão humana (Pedro)')).toBeInTheDocument()
  })

  it('shows the repair queue and session history', async () => {
    renderApp('/auto-reparo')

    expect(await screen.findByText('timeout na coleta')).toBeInTheDocument()
    expect(screen.getByText('Escalado')).toBeInTheDocument()
    const queue = screen.getByRole('region', { name: 'Fila de reparos' })
    expect(within(queue).getAllByText('aguardando slot')).toHaveLength(2)
    expect(screen.getByRole('link', { name: /Ver sessão/ })).toHaveAttribute(
      'href',
      expect.stringContaining('221'),
    )
  })

  it('previews the approval email from the audit screen', async () => {
    renderApp('/')
    const user = userEvent.setup()

    await user.click(await screen.findByRole('link', { name: 'Importex Trading' }))
    await user.click(await screen.findByRole('link', { name: 'Ver e-mail de aprovação' }))

    expect(
      await screen.findByRole('heading', { name: 'Cotação pronta para sua aprovação' }),
    ).toBeInTheDocument()
    expect(screen.getByText('12,0% · R$ 2.492,20')).toBeInTheDocument()
    expect(screen.getByText('dólar armador 5,42 (PTAX 5,38)')).toBeInTheDocument()
    expect(screen.getByText('1 item inferido pelo agente')).toBeInTheDocument()
  })
})
