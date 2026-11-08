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
  const delta = summary.receivedToday - summary.receivedYesterday
  const urgent = summary.mostUrgentSlaRisk
  return (
    <section aria-label="Resumo" className="grid grid-cols-2 gap-3 xl:grid-cols-4">
      <SummaryCard
        label="RFQs hoje"
        value={String(summary.receivedToday)}
        hint={`${delta >= 0 ? '+' : ''}${delta} vs ontem`}
      />
      <SummaryCard
        label="Prontas para 1-clique"
        value={String(summary.readyToSend)}
        hint={`confiança ≥ ${summary.autoApproveConfidencePercent}%`}
      />
      <SummaryCard
        label="Tempo médio de resposta"
        value={`${summary.avgResponseMinutes} min`}
        hint={`meta < ${summary.responseTargetMinutes} min`}
      />
      <SummaryCard
        label="SLA em risco"
        value={String(summary.slaAtRiskCount)}
        hint={
          urgent
            ? `${urgent.customerName} · ${minutesUntil(urgent.slaDueAt, now)} min`
            : 'nenhuma cotação'
        }
        alert={summary.slaAtRiskCount > 0}
      />
    </section>
  )
}
