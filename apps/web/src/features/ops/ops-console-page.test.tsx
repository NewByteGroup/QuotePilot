import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderApp } from '@/test/render'

describe('Ops Console', () => {
  it('shows quarantine, connector health and healing sessions from the ops API', async () => {
    renderApp('/ops')

    expect(await screen.findByText('Cotação importação FCL Santos → Paranaguá')).toBeInTheDocument()
    expect(screen.getByText('sem correspondência')).toBeInTheDocument()
    expect(screen.getByText('múltiplas empresas')).toBeInTheDocument()

    expect(screen.getAllByText('BCB PTAX').length).toBeGreaterThan(0)
    expect(screen.getAllByText('Santos Brasil · tarifas').length).toBeGreaterThan(0)
    expect(screen.getByText('degradado')).toBeInTheDocument()

    expect(screen.getAllByText('Terminal mirror · demo').length).toBeGreaterThan(0)
    expect(screen.getByText('PR pronto')).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: 'PR' })[0]).toHaveAttribute(
      'href',
      'https://github.com/NewByteGroup/QuotePilot/pull/221',
    )
    expect(screen.getAllByRole('link', { name: 'Devin' })[0]).toHaveAttribute(
      'href',
      'https://app.devin.ai/sessions/demo-mirror-221',
    )
  })

  it('exposes the navigation item for a platform admin', async () => {
    renderApp('/')

    expect(await screen.findByRole('link', { name: 'Ops Console' })).toBeInTheDocument()
  })
})
