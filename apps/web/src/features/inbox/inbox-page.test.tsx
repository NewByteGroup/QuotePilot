import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'

import { server } from '@/mocks/node'

import { InboxPage } from './inbox-page'

function renderInbox() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <InboxPage />
    </QueryClientProvider>,
  )
}

describe('InboxPage', () => {
  it('renders summary and quotes from the contract mock', async () => {
    renderInbox()

    expect(await screen.findByText('Importex Trading')).toBeInTheDocument()
    expect(screen.getByText(/atlantico@in\.quotepilot\.app/)).toBeInTheDocument()
    expect(screen.getByText('Atlântico Cargo')).toBeInTheDocument()

    const summary = screen.getByRole('region', { name: 'Resumo' })
    expect(within(summary).getByText('RFQs hoje')).toBeInTheDocument()
    expect(within(summary).getByText('Polímeros Sul · 48 min')).toBeInTheDocument()

    expect(screen.getByText(/R\$\s23\.260,00/)).toBeInTheDocument()
    expect(screen.getByText('Mostrando 14 de 14 cotações')).toBeInTheDocument()
  })

  it('filters by status through the API query', async () => {
    renderInbox()
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Pronto para 1-clique · 3' }))

    expect(await screen.findByText('Mostrando 3 de 14 cotações')).toBeInTheDocument()
    expect(screen.queryByText('Verde Agro Insumos')).not.toBeInTheDocument()
  })

  it('searches by customer name', async () => {
    renderInbox()
    const user = userEvent.setup()

    await user.type(await screen.findByLabelText('Buscar RFQ ou cliente'), 'polímeros')

    expect(await screen.findByText('Mostrando 1 de 1 cotações')).toBeInTheDocument()
    expect(screen.getByText('Polímeros Sul')).toBeInTheDocument()
  })

  it('shows an error when the API fails', async () => {
    server.use(
      http.get('*/v1/inbox/quotes', () =>
        HttpResponse.json({ code: 'internal', message: 'boom' }, { status: 500 }),
      ),
    )
    renderInbox()

    expect(await screen.findByRole('alert')).toHaveTextContent('Não foi possível carregar')
  })
})
