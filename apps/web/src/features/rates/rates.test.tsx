import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderApp } from '@/test/render'

describe('Tarifas & regras', () => {
  it('lists net rate tables and switches tabs', async () => {
    renderApp('/tarifas')
    const user = userEvent.setup()

    expect(await screen.findByText('Maersk')).toBeInTheDocument()
    expect(screen.getByText('Processando 64%')).toBeInTheDocument()
    expect(screen.getByText('Expira em 2 dias')).toBeInTheDocument()
    expect(screen.getByText(/3 cotações usaram snapshot antigo/)).toBeInTheDocument()

    await user.click(screen.getByRole('link', { name: 'Identidade da empresa' }))
    expect(await screen.findByLabelText('CNPJ')).toHaveValue('12.345.678/0001-90')
  })

  it('toggles a pricing automation', async () => {
    renderApp('/tarifas/regras')
    const user = userEvent.setup()

    const toggle = await screen.findByRole('switch', {
      name: 'Recalcular se o câmbio variar > 2%',
    })
    expect(toggle).toBeChecked()
    await user.click(toggle)
    await waitFor(() => expect(toggle).not.toBeChecked())
  })

  it('adds a route margin through the modal', async () => {
    renderApp('/tarifas/regras')
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Adicionar rota' }))
    const dialog = await screen.findByRole('dialog', { name: 'Adicionar rota' })
    await user.selectOptions(within(dialog).getByLabelText('Origem da rota 1'), 'CNNSA')
    await user.selectOptions(within(dialog).getByLabelText('Destino da rota 1'), 'BRSSZ')
    await user.type(within(dialog).getByLabelText('Margem da rota 1'), '10,5')
    await user.click(within(dialog).getByRole('button', { name: 'Salvar rotas' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.getByText('CNNSA → BRSSZ')).toBeInTheDocument()
    expect(screen.getByText('10,5%')).toBeInTheDocument()
  })
})
