import { ChevronLeft } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Link, useParams } from 'react-router'

import { useQuoteDetail } from '@/api/queries'
import { formatDuration, formatReceivedAt, minutesUntil } from '@/features/inbox/format'
import { StatusBadge } from '@/features/inbox/status-badge'
import { formatTime } from '@/lib/format'
import { cn } from '@/lib/utils'

import { EmailPanel } from './email-panel'
import { PricePanel } from './price-panel'

const HIGH_PRIORITY_MINUTES = 90

function SlaBox({ dueAt, atRisk, now }: { dueAt: string; atRisk: boolean; now: Date }) {
  const { t } = useTranslation()
  const minutesLeft = minutesUntil(dueAt, now)
  const high = atRisk || minutesLeft < HIGH_PRIORITY_MINUTES
  return (
    <div
      className={cn(
        'rounded-md border px-3 py-2 text-xs leading-relaxed',
        high ? 'border-red-300 bg-red-50 text-red-700' : 'bg-card text-muted-foreground',
      )}
    >
      <p>{t('audit.slaUntil', { time: formatTime(dueAt) })}</p>
      <p>
        {t('audit.slaRemaining', {
          duration: formatDuration(minutesLeft),
          priority: high ? t('audit.high') : t('audit.normal'),
        })}
      </p>
    </div>
  )
}

export function QuoteAuditPage() {
  const { t } = useTranslation()
  const { quoteId = '' } = useParams()
  const detail = useQuoteDetail(quoteId)
  const now = new Date()

  if (detail.isPending) {
    return <main className="text-muted-foreground flex-1 p-6 text-sm">{t('audit.loading')}</main>
  }

  if (detail.isError) {
    return (
      <main className="flex-1 space-y-3 p-6">
        <p
          role="alert"
          className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700"
        >
          {t('audit.loadError')}
        </p>
        <Link to="/" className="text-primary text-sm hover:underline">
          {t('audit.back')}
        </Link>
      </main>
    )
  }

  const { quote, email, extraction, pricing } = detail.data
  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="space-y-1">
          <div className="flex flex-wrap items-center gap-3">
            <Link
              to="/"
              aria-label={t('audit.back')}
              className="text-muted-foreground hover:text-foreground"
            >
              <ChevronLeft className="size-5" />
            </Link>
            <h1 className="text-2xl font-bold tracking-tight">
              RFQ #{quote.rfqNumber} · {quote.customerName}
            </h1>
            <StatusBadge status={quote.status} />
          </div>
          <p className="text-muted-foreground pl-8 text-sm">
            {t('audit.received', {
              subject: quote.subject,
              when: formatReceivedAt(quote.receivedAt, now),
            })}
          </p>
          <Link
            to={`/quotes/${quote.id}/aprovacao`}
            className="text-primary pl-8 text-sm hover:underline"
          >
            {t('audit.approvalEmailLink')}
          </Link>
        </div>
        {quote.slaDueAt ? (
          <SlaBox dueAt={quote.slaDueAt} atRisk={quote.slaAtRisk} now={now} />
        ) : null}
      </header>

      <div className="grid items-start gap-4 xl:grid-cols-2">
        <EmailPanel email={email} extraction={extraction} now={now} />
        <PricePanel pricing={pricing} status={quote.status} />
      </div>
    </main>
  )
}
