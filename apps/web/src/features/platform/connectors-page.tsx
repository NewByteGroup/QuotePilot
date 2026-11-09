import type { Schemas } from '@/api/client'
import { useConnectorOverview } from '@/api/queries'
import { PageHeader, Panel, QueryFallback, StatCard } from '@/components/panel'
import { StatusPill } from '@/components/status-pill'
import { formatDecimal, formatPercent, formatTime } from '@/lib/format'
import { cn } from '@/lib/utils'

import { RepairTimeline } from './repair-timeline'

const HEALTHY_PERCENT = 95

const KIND_LABELS: Record<Schemas['ConnectorKind'], string> = {
  fx: 'câmbio',
  public_tariff: 'tarifa pública',
  fx_reference: 'câmbio referência',
}

function ConnectorCard({ connector }: { connector: Schemas['Connector'] }) {
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
          <StatusPill tone="neutral">{KIND_LABELS[connector.kind]}</StatusPill>
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
          latência {formatDecimal(connector.latencyMs / 1000, 1)}s · coleta{' '}
          {formatTime(connector.collectedAt)}
          {connector.detail ? ` · ${connector.detail}` : null}
        </span>
        {connector.repairing ? <span className="text-amber-700">Devin acionado</span> : null}
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
  if (names.length === 0) return 'todas as fontes saudáveis'
  const noun = names.length === 1 ? 'fonte degradada' : 'fontes degradadas'
  return `${names.length} ${noun} (${names.join(', ')})`
}

export function ConnectorsPage() {
  const overview = useConnectorOverview()
  const data = overview.data

  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <PageHeader
        title="Console de conectores"
        subtitle="Inteligência pública compartilhada · snapshots imutáveis referenciados por public_snapshot_id"
      />
      {data ? (
        <>
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <StatCard
              label="Fontes ativas"
              value={String(data.stats.activeSources)}
              hint={data.stats.sourceKindsSummary}
            />
            <StatCard
              label="Saúde média"
              value={formatPercent(data.stats.averageHealthPercent)}
              hint={degradedHint(data.stats.degradedSourceNames)}
            />
            <StatCard
              label="Reparos automáticos (7d)"
              value={String(data.stats.repairs7d)}
              hint={`tempo médio ${data.stats.repairMeanMinutes} min`}
            />
            <StatCard
              label="Snapshots (24h)"
              value={data.stats.snapshots24h.toLocaleString('pt-BR')}
              hint={`${data.stats.collectionFailures24h} falhas de coleta`}
            />
          </div>
          <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
            {data.connectors.map((connector) => (
              <ConnectorCard key={connector.id} connector={connector} />
            ))}
          </div>
          {data.liveRepair ? (
            <Panel
              title="Auto-reparo · Devin live"
              badge={
                <span className="bg-muted rounded-sm px-1.5 py-0.5 text-xs">
                  conector {data.liveRepair.connectorName}
                </span>
              }
            >
              <RepairTimeline timeline={data.liveRepair} />
            </Panel>
          ) : null}
        </>
      ) : (
        <QueryFallback isError={overview.isError} what="os conectores" />
      )}
    </main>
  )
}
