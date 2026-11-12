import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { renderApp } from '@/test/render'

// Tests load no CSS, so the `hidden`/`lg:` Tailwind classes hide nothing: the
// mobile top bar and the desktop aside are always in the DOM. Scope assertions
// to the drawer while it is open.
describe('Menu mobile', () => {
  it('shows only the trigger and the desktop org card while closed', async () => {
    renderApp('/')

    expect(await screen.findByRole('button', { name: 'Abrir menu' })).toBeInTheDocument()
    expect(await screen.findAllByRole('button', { name: 'Trocar de empresa' })).toHaveLength(1)
  })

  it('opens the drawer with the nav links, org card and settings', async () => {
    renderApp('/')
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Abrir menu' }))
    const drawer = await screen.findByRole('dialog', { name: 'Menu' })

    expect(within(drawer).getByRole('link', { name: 'Tarifas & regras' })).toBeInTheDocument()
    expect(within(drawer).getByRole('link', { name: 'Conectores' })).toBeInTheDocument()
    expect(within(drawer).getByRole('link', { name: 'Auto-reparo' })).toBeInTheDocument()
    expect(within(drawer).getByRole('button', { name: 'Trocar de empresa' })).toBeInTheDocument()
    expect(within(drawer).getByRole('button', { name: 'Configurações' })).toBeInTheDocument()
    // The desktop aside's card stays mounted (aria-hidden to AT): two in the DOM.
    expect(screen.getAllByRole('button', { name: 'Trocar de empresa', hidden: true })).toHaveLength(
      2,
    )
  })

  it('navigates and closes the drawer when a link is clicked', async () => {
    renderApp('/')
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Abrir menu' }))
    const drawer = await screen.findByRole('dialog', { name: 'Menu' })
    await user.click(within(drawer).getByRole('link', { name: 'Conectores' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(
      await screen.findByRole('heading', { name: 'Console de conectores' }),
    ).toBeInTheDocument()
  })

  it('closes the drawer after the organization is switched inside it', async () => {
    renderApp('/')
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Abrir menu' }))
    const drawer = await screen.findByRole('dialog', { name: 'Menu' })
    await user.click(within(drawer).getByRole('button', { name: 'Trocar de empresa' }))

    // The dropdown portals outside the dialog, so the modal marks it aria-hidden.
    await user.click(
      await screen.findByRole('menuitemradio', { name: /Pacífico Log/, hidden: true }),
    )

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(JSON.parse(localStorage.getItem('quotepilot.mock.session') ?? '{}')).toMatchObject({
      organization: { name: 'Pacífico Log' },
    })
  })
})
