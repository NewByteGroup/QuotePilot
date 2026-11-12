import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'

import { server } from '@/mocks/node'
import { buildCompanyIdentity } from '@/mocks/platform-fixtures'
import { renderApp } from '@/test/render'

const saveButton = () => screen.getByRole('button', { name: 'Salvar alterações' })

describe('Identidade da empresa', () => {
  it('validates the CNPJ and saves company data with one button', async () => {
    renderApp('/tarifas/identidade')
    const user = userEvent.setup()

    const legalName = await screen.findByLabelText('Razão social')
    expect(saveButton()).toBeDisabled()

    await user.clear(legalName)
    await user.type(legalName, 'Atlântico Cargo S.A.')
    const taxId = screen.getByLabelText('CNPJ')
    await user.clear(taxId)
    await user.type(taxId, '123')
    expect(screen.getByRole('status')).toHaveTextContent('alterações não salvas')

    await user.click(saveButton())
    expect(screen.getByRole('alert')).toHaveTextContent('CNPJ inválido')

    await user.clear(taxId)
    await user.type(taxId, '98765432000110')
    expect(taxId).toHaveValue('98.765.432/0001-10')
    await user.click(saveButton())

    expect(await screen.findByText('Alterações salvas.')).toBeInTheDocument()
    expect(saveButton()).toBeDisabled()
    expect(legalName).toHaveValue('Atlântico Cargo S.A.')
    expect(
      within(screen.getByRole('complementary')).getByText('Atlântico Cargo S.A.'),
    ).toBeVisible()
    expect(localStorage.getItem('quotepilot.mock.company-identity')).toContain(
      'Atlântico Cargo S.A.',
    )
  })

  it('stages the primary color until saved', async () => {
    renderApp('/tarifas/identidade')
    const user = userEvent.setup()

    const blue = await screen.findByRole('radio', { name: 'Cor #2563eb' })
    await user.click(blue)
    expect(blue).toHaveAttribute('aria-checked', 'true')
    expect(screen.getByRole('status')).toHaveTextContent('alterações não salvas')

    await user.click(saveButton())
    expect(await screen.findByText('Alterações salvas.')).toBeInTheDocument()
    expect(blue).toHaveAttribute('aria-checked', 'true')
  })

  it('discards unsaved changes', async () => {
    renderApp('/tarifas/identidade')
    const user = userEvent.setup()

    const details = await screen.findByLabelText('Detalhes')
    await user.clear(details)
    await user.type(details, 'Santos · SP')
    await user.click(screen.getByRole('button', { name: 'Descartar' }))

    expect(details).toHaveValue('São Paulo · SP · Brasil · (11) 0000-0000')
    expect(saveButton()).toBeDisabled()
  })

  it('uploads and removes the letterhead logo on save', async () => {
    // jsdom's File/FormData can't be sent through Node's fetch, so the upload response is stubbed.
    server.use(
      http.put('*/v1/organization/identity/letterhead-logo', () =>
        HttpResponse.json({
          ...buildCompanyIdentity(),
          letterheadLogoUrl: 'data:image/svg+xml;base64,PHN2Zy8+',
        }),
      ),
    )
    renderApp('/tarifas/identidade')
    const user = userEvent.setup()

    await user.upload(
      await screen.findByLabelText('Arquivo do logo'),
      new File(['<svg xmlns="http://www.w3.org/2000/svg"/>'], 'logo.svg', {
        type: 'image/svg+xml',
      }),
    )
    expect(await screen.findByRole('img', { name: 'Logo da empresa' })).toBeInTheDocument()
    await user.click(saveButton())
    await waitFor(() =>
      expect(screen.getByRole('img', { name: 'Logo da empresa' })).toHaveAttribute(
        'src',
        'data:image/svg+xml;base64,PHN2Zy8+',
      ),
    )

    await user.click(screen.getByRole('button', { name: 'Remover' }))
    expect(screen.queryByRole('img', { name: 'Logo da empresa' })).not.toBeInTheDocument()
    await user.click(saveButton())
    expect(await screen.findByText('Alterações salvas.')).toBeInTheDocument()
  })

  it('rejects a logo larger than 2 MB', async () => {
    renderApp('/tarifas/identidade')
    const user = userEvent.setup()

    await user.upload(
      await screen.findByLabelText('Arquivo do logo'),
      new File([new Uint8Array(3 * 1024 * 1024)], 'big.png', { type: 'image/png' }),
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('até 2 MB')
  })
})
