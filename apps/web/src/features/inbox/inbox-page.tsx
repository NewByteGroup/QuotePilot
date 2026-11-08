import { RefreshCw, Search } from 'lucide-react'
import { useDeferredValue, useState } from 'react'

import { useInboxQuotes, useSession } from '@/api/queries'
import { AppSidebar } from '@/components/layout/app-sidebar'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

import { formatRelative } from './format'
import { QuotesTable } from './quotes-table'
import { StatusFilters, type StatusFilter } from './status-filters'
import { SummaryCards } from './summary-cards'

export function InboxPage() {
  const [status, setStatus] = useState<StatusFilter>('all')
  const [search, setSearch] = useState('')
  const deferredSearch = useDeferredValue(search)

  const session = useSession()
  const inbox = useInboxQuotes({
    status: status === 'all' ? undefined : status,
    q: deferredSearch,
  })

  const page = inbox.data
  const now = new Date()
  const pendingCount = page
    ? page.statusCounts.awaiting_review + page.statusCounts.ready_to_send
    : undefined

  return (
    <div className="flex min-h-svh">
      <AppSidebar session={session.data} inboxPendingCount={pendingCount} />
      <main className="min-w-0 flex-1 space-y-5 p-6">
        <header className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">Inbox de cotações</h1>
            <p className="text-muted-foreground text-sm">
              RFQs recebidos em {session.data?.organization.inboundAddress ?? '…'}
              {page ? ` · atualizado ${formatRelative(page.refreshedAt, now)}` : null}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <div className="relative">
              <Search className="text-muted-foreground pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2" />
              <Input
                type="search"
                aria-label="Buscar RFQ ou cliente"
                placeholder="Buscar RFQ, cliente..."
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                className="bg-card w-56 pl-9"
              />
            </div>
            <Button variant="outline" onClick={() => inbox.refetch()} disabled={inbox.isFetching}>
              <RefreshCw className={inbox.isFetching ? 'animate-spin' : undefined} />
              Atualizar
            </Button>
          </div>
        </header>

        {inbox.isError ? (
          <p
            role="alert"
            className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700"
          >
            Não foi possível carregar as cotações. Tente atualizar.
          </p>
        ) : null}

        {page ? (
          <>
            <SummaryCards summary={page.summary} now={now} />
            <StatusFilters counts={page.statusCounts} value={status} onChange={setStatus} />
            <QuotesTable quotes={page.items} totalCount={page.statusCounts.all} now={now} />
          </>
        ) : inbox.isPending ? (
          <p className="text-muted-foreground text-sm">Carregando cotações…</p>
        ) : null}
      </main>
    </div>
  )
}
