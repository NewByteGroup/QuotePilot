import { useTranslation } from 'react-i18next'

import type { Schemas } from '@/api/client'
import { useRepairOverview } from '@/api/queries'
import { PageHeader, Panel, QueryFallback, StatCard } from '@/components/panel'
import { StatusPill, type PillTone } from '@/components/status-pill'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { formatReceivedAt } from '@/features/inbox/format'
import i18n from '@/i18n'
import { formatTime } from '@/lib/format'

import { RepairTimeline } from './repair-timeline'

const QUEUE_TONES: Record<Schemas['RepairQueueItem']['state'], PillTone> = {
  in_progress: 'violet',
  waiting_slot: 'neutral',
  scheduled: 'outline',
}

const RESULT_TONES: Record<Schemas['RepairSession']['result'], PillTone> = {
  resolved: 'green',
  escalated: 'amber',
}

function queueLabel(item: Schemas['RepairQueueItem']): string {
  const label = i18n.t(`repairs.${item.state}`)
  return item.state === 'scheduled' && item.scheduledAt
    ? `${label} ${formatTime(item.scheduledAt)}`
    : label
}

export function RepairsPage() {
  const { t } = useTranslation()
  const overview = useRepairOverview()
  const data = overview.data
  const now = new Date()

  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <PageHeader title={t('repairs.title')} subtitle={t('repairs.subtitle')} />
      {data ? (
        <>
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <StatCard
              label={t('repairs.repairs7d')}
              value={String(data.stats.repairs7d)}
              hint={
                data.stats.escalated7d === 0
                  ? t('repairs.allDone')
                  : t('repairs.escalatedCount', { count: data.stats.escalated7d })
              }
            />
            <StatCard
              label={t('repairs.successRate')}
              value={`${data.stats.successRatePercent}%`}
              hint={
                data.stats.escalated7d === 0
                  ? t('repairs.noEscalation')
                  : t('repairs.escalatedHuman', { count: data.stats.escalated7d })
              }
            />
            <StatCard
              label={t('repairs.mttr')}
              value={t('common.minutes', { count: data.stats.mttrMinutes })}
              hint={t('repairs.mttrHint')}
            />
            <StatCard
              label={t('repairs.queued')}
              value={String(data.stats.queued)}
              hint={t('repairs.queuedHint')}
            />
          </div>

          <div className="grid items-start gap-4 xl:grid-cols-[2fr_1fr]">
            <Panel
              title={t('repairs.current')}
              description={
                data.current
                  ? t('connectors.connector', { name: data.current.connectorName })
                  : undefined
              }
            >
              {data.current ? (
                <RepairTimeline timeline={data.current} highlightAnomaly showSessionLink />
              ) : (
                <p className="text-muted-foreground p-4 text-sm">{t('repairs.noCurrent')}</p>
              )}
            </Panel>
            <Panel title={t('repairs.queue')}>
              <ul className="divide-y">
                {data.queue.map((item) => (
                  <li
                    key={item.connectorName}
                    className="flex items-center justify-between gap-3 px-4 py-3"
                  >
                    <div className="leading-tight">
                      <p className="text-sm">{item.connectorName}</p>
                      <p className="text-muted-foreground text-xs">{queueLabel(item)}</p>
                    </div>
                    <StatusPill tone={QUEUE_TONES[item.state]}>{queueLabel(item)}</StatusPill>
                  </li>
                ))}
              </ul>
            </Panel>
          </div>

          <Panel title={t('repairs.history')}>
            <Table>
              <TableHeader>
                <TableRow className="text-xs hover:bg-transparent">
                  <TableHead className="pl-4">{t('repairs.session')}</TableHead>
                  <TableHead>{t('repairs.connector')}</TableHead>
                  <TableHead>{t('repairs.anomaly')}</TableHead>
                  <TableHead>{t('repairs.result')}</TableHead>
                  <TableHead className="text-right">{t('repairs.duration')}</TableHead>
                  <TableHead className="pr-4 text-right">{t('repairs.when')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.history.map((session) => (
                  <TableRow key={session.sessionNumber}>
                    <TableCell className="text-muted-foreground pl-4">
                      #{session.sessionNumber}
                    </TableCell>
                    <TableCell className="font-medium">{session.connectorName}</TableCell>
                    <TableCell>{session.anomaly}</TableCell>
                    <TableCell>
                      <StatusPill tone={RESULT_TONES[session.result]}>
                        {t(`repairs.${session.result}`)}
                      </StatusPill>
                    </TableCell>
                    <TableCell className="text-right font-semibold">
                      {t('common.minutes', { count: session.durationMinutes })}
                    </TableCell>
                    <TableCell className="text-muted-foreground pr-4 text-right text-xs">
                      {formatReceivedAt(session.finishedAt, now)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Panel>
        </>
      ) : (
        <QueryFallback isError={overview.isError} what={t('repairs.what')} />
      )}
    </main>
  )
}
