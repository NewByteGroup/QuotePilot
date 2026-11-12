import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderApp } from '@/test/render'

describe('Troca de empresa', () => {
  it('switches the active organization from the menu under the company card', async () => {
    renderApp('/tarifas/identidade')
    expect(await screen.findByLabelText('CNPJ')).toHaveValue('12.345.678/0001-90')

    await userEvent.click(screen.getByRole('button', { name: 'Trocar de empresa' }))
    const menu = await screen.findByRole('menu')
    expect(within(menu).getByRole('menuitemradio', { name: /Atlântico Cargo/ })).toBeChecked()

    await userEvent.click(within(menu).getByRole('menuitemradio', { name: /Pacífico Log/ }))
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument())

    expect(await screen.findByText('Pacífico Log Ltda')).toBeInTheDocument()
    expect(screen.getByText('Plano Essencial')).toBeInTheDocument()
    expect(
      await screen.findByText('RFQs recebidos em pacifico@in.quotepilot.app'),
    ).toBeInTheDocument()
    expect(await screen.findByText(/Mostrando 7 de 7 cotações/)).toBeInTheDocument()

    expect(JSON.parse(localStorage.getItem('quotepilot.mock.session') ?? '{}')).toMatchObject({
      organization: { name: 'Pacífico Log' },
    })
  })
})
