import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderApp } from '@/test/render'

describe('QuoteAuditPage', () => {
  it('opens the audit of the clicked quote and goes back to the inbox', async () => {
    renderApp('/')
    const user = userEvent.setup()

    await user.click(await screen.findByRole('link', { name: 'Importex Trading' }))

    expect(
      await screen.findByRole('heading', { name: 'RFQ #2481 · Importex Trading' }),
    ).toBeInTheDocument()
    expect(screen.getByText('compras@importex.com.br')).toBeInTheDocument()
    expect(screen.getByText('packing-list.pdf')).toBeInTheDocument()

    const extraction = screen.getByRole('list', { name: 'Extração do agente' })
    expect(within(extraction).getByText('8479.89.99')).toBeInTheDocument()

    const fees = screen.getByRole('region', { name: 'Taxas locais · Santos' })
    expect(within(fees).getByText('THC destino')).toBeInTheDocument()
    expect(screen.getByText(/R\$\s20\.768,35/)).toBeInTheDocument()
    expect(screen.getByText(/R\$\s2\.492,20/)).toBeInTheDocument()

    await user.click(screen.getByRole('link', { name: 'Voltar para a inbox' }))
    expect(await screen.findByRole('heading', { name: 'Inbox de cotações' })).toBeInTheDocument()
  })

  it('opens a quote by clicking the subject row', async () => {
    renderApp('/')
    const user = userEvent.setup()

    await user.click(await screen.findByText('Ureia granulada 25 t · Paranaguá'))

    expect(
      await screen.findByRole('heading', { name: 'RFQ #2480 · Verde Agro Insumos' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Taxas locais · Paranaguá' })).toBeInTheDocument()
  })

  it('shows an error for an unknown quote', async () => {
    renderApp('/quotes/00000000-0000-4000-8000-000000000000')

    expect(await screen.findByRole('alert')).toHaveTextContent('Não foi possível carregar')
  })
})
