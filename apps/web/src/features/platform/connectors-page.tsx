import { useTranslation } from 'react-i18next'

import type { Schemas } from '@/api/client'
import { useConnectorOverview } from '@/api/queries'
import { PageHeader, Panel, QueryFallback, StatCard } from '@/components/panel'
import { StatusPill } from '@/components/status-pill'
import i18n, { currentLocale } from '@/i18n'
import { formatDecimal, formatPercent, formatTime } from '@/lib/format'
import { cn } from '@/lib/utils'

import { RepairTimeline } from './repair-timeline'

const HEALTHY_PERCENT = 95

function ConnectorCard({ connector }: { connector: Schemas['Connector'] }) {
  const { t } = useTranslation()
  const healthy = connector.healthPercent >= HEALTHY_PERCENT
  return (
    <article
      aria-label={connector.name}
      className={cn(
        'space-y-3 rounded-lg border p-4',
        connector.repairing ? 'border-amber-400 bg-amber-50/60' : 'bg-card',
      )}
    >
      <div className="flex justify-between gap-3">
        <div className="space-y-1.5">
          <h3 className="text-sm font-semibold">{connector.name}</h3>
          <StatusPill tone="neutral">{t(`connectors.kind_${connector.kind}`)}</StatusPill>
        </div>
        <span
          className={cn(
            'flex items-center gap-1.5 text-sm font-semibold',
            healthy ? 'text-green-700' : 'text-amber-600',
          )}
        >
          <span
            aria-hidden
            className={cn('size-1.5 rounded-full', healthy ? 'bg-green-600' : 'bg-amber-500')}
          />
          {connector.healthPercent}%
        </span>
      </div>
      <div className="text-muted-foreground flex justify-between gap-3 text-xs">
        <span>
          {t('connectors.latency', {
            seconds: formatDecimal(connector.latencyMs / 1000, 1),
            time: formatTime(connector.collectedAt),
          })}
          {connector.detail ? ` · ${connector.detail}` : null}
        </span>
        {connector.repairing ? (
          <span className="text-amber-700">{t('connectors.devinTriggered')}</span>
        ) : null}
      </div>
      <div className="bg-muted h-1 rounded-full">
        <div
          className={cn('h-1 rounded-full', healthy ? 'bg-green-600' : 'bg-amber-500')}
          style={{ width: `${connector.healthPercent}%` }}
        />
      </div>
    </article>
  )
}

function degradedHint(names: string[]): string {
  if (names.length === 0) return i18n.t('connectors.allHealthy')
  return i18n.t('connectors.degraded', { count: names.length, names: names.join(', ') })
}

export function ConnectorsPage() {
  const { t } = useTranslation()
  const overview = useConnectorOverview()
  const data = overview.data

  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <PageHeader title={t('connectors.title')} subtitle={t('connectors.subtitle')} />
      {data ? (
        <>
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <StatCard
              label={t('connectors.activeSources')}
              value={String(data.stats.activeSources)}
              hint={data.stats.sourceKindsSummary}
            />
            <StatCard
              label={t('connectors.avgHealth')}
              value={formatPercent(data.stats.averageHealthPercent)}
              hint={degradedHint(data.stats.degradedSourceNames)}
            />
            <StatCard
              label={t('connectors.repairs7d')}
              value={String(data.stats.repairs7d)}
              hint={t('connectors.repairMean', { count: data.stats.repairMeanMinutes })}
            />
            <StatCard
              label={t('connectors.snapshots')}
              value={data.stats.snapshots24h.toLocaleString(currentLocale())}
              hint={t('connectors.failures', { count: data.stats.collectionFailures24h })}
            />
          </div>
          <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
            {data.connectors.map((connector) => (
              <ConnectorCard key={connector.id} connector={connector} />
            ))}
          </div>
          {data.liveRepair ? (
            <Panel
              title={t('connectors.liveRepair')}
              badge={
                <span className="bg-muted rounded-sm px-1.5 py-0.5 text-xs">
                  {t('connectors.connector', { name: data.liveRepair.connectorName })}
                </span>
              }
            >
              <RepairTimeline timeline={data.liveRepair} />
            </Panel>
          ) : null}
        </>
      ) : (
        <QueryFallback isError={overview.isError} what={t('connectors.what')} />
      )}
    </main>
  )
}
