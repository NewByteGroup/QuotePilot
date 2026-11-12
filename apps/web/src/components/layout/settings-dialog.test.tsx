import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderApp } from '@/test/render'

const openSettings = async () => {
  await userEvent.click(await screen.findByRole('button', { name: 'Configurações' }))
  return screen.getByRole('dialog', { name: 'Configurações' })
}

describe('Configurações do usuário', () => {
  it('renames the user and sends the language to the backend', async () => {
    renderApp('/')
    const dialog = await openSettings()

    const email = within(dialog).getByLabelText('E-mail')
    expect(email).toHaveValue('marcelo.corner@atlanticocargo.com.br')
    expect(email).toHaveAttribute('readonly')

    const name = within(dialog).getByLabelText('Nome')
    await userEvent.clear(name)
    await userEvent.type(name, 'Marcelo C.')

    await userEvent.click(within(dialog).getByRole('button', { name: /Idioma/ }))
    const picker = screen.getByRole('dialog', { name: 'Idioma' })
    expect(within(picker).getByRole('radio', { name: /Português/ })).toBeChecked()
    await userEvent.click(within(picker).getByRole('radio', { name: /English/ }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Idioma' })).toBeNull())
    expect(within(dialog).getByRole('button', { name: /Idioma/ })).toHaveTextContent('English')

    await userEvent.click(within(dialog).getByRole('button', { name: 'Salvar' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    expect(screen.getByText('Marcelo C.')).toBeInTheDocument()
    expect(JSON.parse(localStorage.getItem('quotepilot.mock.session') ?? '{}')).toMatchObject({
      user: { name: 'Marcelo C.', language: 'en' },
    })
  })

  it('does not save an empty name', async () => {
    renderApp('/')
    const dialog = await openSettings()
    await userEvent.clear(within(dialog).getByLabelText('Nome'))
    expect(within(dialog).getByRole('button', { name: 'Salvar' })).toBeDisabled()
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Informe um nome.')
  })
})
