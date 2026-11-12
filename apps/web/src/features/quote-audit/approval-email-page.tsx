import { ChevronLeft } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Link, useParams } from 'react-router'

import { useCompanyIdentity, useQuoteDetail, useSession } from '@/api/queries'
import { QueryFallback } from '@/components/panel'
import { Button } from '@/components/ui/button'
import { equipmentLabel, formatMoney } from '@/features/inbox/format'
import { formatDecimal, formatPercent, initials } from '@/lib/format'
import { cn } from '@/lib/utils'

function SummaryRow({ label, value, strong }: { label: string; value: string; strong?: boolean }) {
  return (
    <div className="flex items-center justify-between gap-4 px-4 py-3">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className={cn('text-right', strong ? 'text-base font-bold' : 'text-xs font-medium')}>
        {value}
      </dd>
    </div>
  )
}

export function ApprovalEmailPage() {
  const { t } = useTranslation()
  const { quoteId = '' } = useParams()
  const detail = useQuoteDetail(quoteId)
  const session = useSession()
  const identity = useCompanyIdentity()

  if (!detail.data) {
    return (
      <main className="flex-1 p-6">
        <QueryFallback isError={detail.isError} what={t('approval.what')} />
      </main>
    )
  }

  const { quote, pricing } = detail.data
  const organizationName = session.data?.organization.name
  const inferredItems =
    pricing?.sections.flatMap((section) => section.items).filter((i) => i.inferred) ?? []
  const auditPath = `/quotes/${quote.id}`
  const brandColor = identity.data?.primaryColor ?? '#0f172a'
  const logoUrl = identity.data?.letterheadLogoUrl

  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <header className="flex items-center gap-3">
        <Link
          to={auditPath}
          aria-label={t('approval.back')}
          className="text-muted-foreground hover:text-foreground"
        >
          <ChevronLeft className="size-5" />
        </Link>
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t('approval.title')}</h1>
          <p className="text-muted-foreground text-sm">{t('approval.subtitle')}</p>
        </div>
      </header>

      <article
        aria-label={t('approval.preview')}
        className="bg-card mx-auto max-w-xl overflow-hidden rounded-xl border"
      >
        <header
          className="flex items-center gap-3 px-6 py-4 text-white"
          style={{ backgroundColor: brandColor }}
        >
          {logoUrl ? (
            <img src={logoUrl} alt="" className="size-9 rounded-md bg-white object-contain p-1" />
          ) : (
            <span className="flex size-9 items-center justify-center rounded-md bg-white/10 text-sm font-semibold">
              {initials(organizationName)}
            </span>
          )}
          <div className="leading-tight">
            <p className="font-semibold">{organizationName ?? '…'}</p>
            <p className="text-xs text-white/70">{t('approval.via')}</p>
          </div>
        </header>

        <div className="space-y-5 px-6 py-6">
          <div>
            <h2 className="text-xl font-bold">{t('approval.heading')}</h2>
            <p className="text-muted-foreground text-sm">
              RFQ #{quote.rfqNumber} · {quote.customerName} · {quote.route.origin} →{' '}
              {quote.route.destination} ({equipmentLabel(quote.equipment)})
            </p>
          </div>

          {pricing ? (
            <>
              <dl className="bg-muted/40 divide-y rounded-lg border text-sm">
                <SummaryRow
                  label={t('price.proposal')}
                  value={formatMoney(pricing.proposalTotal)}
                  strong
                />
                <SummaryRow
                  label={t('approval.netMargin')}
                  value={`${formatPercent(pricing.marginPercent)} · ${formatMoney(pricing.margin)}`}
                />
                <SummaryRow
                  label={t('approval.fx')}
                  value={`${pricing.fx.label} ${formatDecimal(pricing.fx.rate)} (${pricing.fx.referenceLabel} ${formatDecimal(pricing.fx.referenceRate)})`}
                />
                <SummaryRow
                  label={t('approval.validity')}
                  value={t('approval.validityValue', {
                    hours: pricing.validityHours,
                    threshold: formatPercent(pricing.recalcFxThresholdPercent, 0),
                  })}
                />
              </dl>

              {inferredItems.length > 0 ? (
                <div
                  role="note"
                  className="rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 text-sm"
                >
                  <p className="font-medium text-amber-800">
                    {t('approval.inferred', { count: inferredItems.length })}
                  </p>
                  {inferredItems.map((item) => (
                    <p key={item.label} className="text-amber-700">
                      {item.label}
                      {item.detail ? ` — ${item.detail}` : null} — {t('approval.reviewSuffix')}
                    </p>
                  ))}
                </div>
              ) : null}

              <div className="flex gap-2">
                <Button
                  className="flex-1 text-white hover:opacity-90"
                  style={{ backgroundColor: brandColor }}
                  disabled
                  title={t('price.actionPending')}
                >
                  {t('approval.approve')}
                </Button>
                <Button variant="outline" asChild>
                  <Link to={auditPath}>{t('approval.viewCalc')}</Link>
                </Button>
              </div>
              <p className="text-muted-foreground text-center text-xs">{t('approval.tokenNote')}</p>
            </>
          ) : (
            <p className="text-muted-foreground text-sm">{t('approval.processing')}</p>
          )}
        </div>

        <footer className="text-muted-foreground border-t px-6 py-4 text-center text-xs">
          <p>{t('approval.footerReason', { org: organizationName ?? '…' })}</p>
          <p>{t('approval.footerLgpd')}</p>
        </footer>
      </article>
    </main>
  )
}
