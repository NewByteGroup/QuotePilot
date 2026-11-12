import { useTranslation } from 'react-i18next'

import type { Schemas } from '@/api/client'
import { Card, CardContent } from '@/components/ui/card'

import { minutesUntil } from './format'

function SummaryCard({
  label,
  value,
  hint,
  alert,
}: {
  label: string
  value: string
  hint: string
  alert?: boolean
}) {
  return (
    <Card className="gap-0 rounded-lg py-0 shadow-none">
      <CardContent className="space-y-2 p-4">
        <p className="text-muted-foreground flex items-center gap-1.5 text-xs">
          {alert ? <span aria-hidden className="size-1.5 rounded-full bg-red-600" /> : null}
          {label}
        </p>
        <p className="text-xl font-bold">{value}</p>
        <p className="text-muted-foreground truncate text-xs">{hint}</p>
      </CardContent>
    </Card>
  )
}

export function SummaryCards({ summary, now }: { summary: Schemas['InboxSummary']; now: Date }) {
  const { t } = useTranslation()
  const delta = summary.receivedToday - summary.receivedYesterday
  const urgent = summary.mostUrgentSlaRisk
  return (
    <section
      aria-label={t('inbox.summary.label')}
      className="grid grid-cols-2 gap-3 xl:grid-cols-4"
    >
      <SummaryCard
        label={t('inbox.summary.today')}
        value={String(summary.receivedToday)}
        hint={t('inbox.summary.vsYesterday', { delta: `${delta >= 0 ? '+' : ''}${delta}` })}
      />
      <SummaryCard
        label={t('inbox.summary.ready')}
        value={String(summary.readyToSend)}
        hint={t('inbox.summary.confidence', { percent: summary.autoApproveConfidencePercent })}
      />
      <SummaryCard
        label={t('inbox.summary.avgResponse')}
        value={t('common.minutes', { count: summary.avgResponseMinutes })}
        hint={t('inbox.summary.target', { count: summary.responseTargetMinutes })}
      />
      <SummaryCard
        label={t('inbox.summary.slaRisk')}
        value={String(summary.slaAtRiskCount)}
        hint={
          urgent
            ? `${urgent.customerName} · ${t('common.minutes', { count: minutesUntil(urgent.slaDueAt, now) })}`
            : t('inbox.summary.noQuote')
        }
        alert={summary.slaAtRiskCount > 0}
      />
    </section>
  )
}
