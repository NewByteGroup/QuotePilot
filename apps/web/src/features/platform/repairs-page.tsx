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
import { formatTime } from '@/lib/format'

import { RepairTimeline } from './repair-timeline'

const QUEUE_TONES: Record<Schemas['RepairQueueItem']['state'], PillTone> = {
  in_progress: 'violet',
  waiting_slot: 'neutral',
  scheduled: 'outline',
}

const QUEUE_LABELS: Record<Schemas['RepairQueueItem']['state'], string> = {
  in_progress: 'em andamento',
  waiting_slot: 'aguardando slot',
  scheduled: 'agendado',
}

const RESULT_TONES: Record<Schemas['RepairSession']['result'], PillTone> = {
  resolved: 'green',
  escalated: 'amber',
}

const RESULT_LABELS: Record<Schemas['RepairSession']['result'], string> = {
  resolved: 'Resolvido',
  escalated: 'Escalado',
}

function queueLabel(item: Schemas['RepairQueueItem']): string {
  const label = QUEUE_LABELS[item.state]
  return item.state === 'scheduled' && item.scheduledAt
    ? `${label} ${formatTime(item.scheduledAt)}`
    : label
}

export function RepairsPage() {
  const overview = useRepairOverview()
  const data = overview.data
  const now = new Date()

  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <PageHeader
        title="Auto-reparo"
        subtitle="Agente de reparo automático · histórico e auditoria de sessões"
      />
      {data ? (
        <>
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            <StatCard
              label="Reparos (7d)"
              value={String(data.stats.repairs7d)}
              hint={
                data.stats.escalated7d === 0
                  ? 'todos concluídos'
                  : `${data.stats.escalated7d} escalados`
              }
            />
            <StatCard
              label="Taxa de sucesso"
              value={`${data.stats.successRatePercent}%`}
              hint={
                data.stats.escalated7d === 0
                  ? 'sem escalonamento a humano'
                  : `${data.stats.escalated7d} escalados a humano`
              }
            />
            <StatCard
              label="MTTR"
              value={`${data.stats.mttrMinutes} min`}
              hint="tempo médio de reparo"
            />
            <StatCard
              label="Em fila"
              value={String(data.stats.queued)}
              hint="aguardando slot do Devin"
            />
          </div>

          <div className="grid items-start gap-4 xl:grid-cols-[2fr_1fr]">
            <Panel
              title="Reparo em andamento"
              description={data.current ? `conector ${data.current.connectorName}` : undefined}
            >
              {data.current ? (
                <RepairTimeline timeline={data.current} highlightAnomaly showSessionLink />
              ) : (
                <p className="text-muted-foreground p-4 text-sm">Nenhum reparo em andamento.</p>
              )}
            </Panel>
            <Panel title="Fila de reparos">
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

          <Panel title="Histórico de sessões">
            <Table>
              <TableHeader>
                <TableRow className="text-xs hover:bg-transparent">
                  <TableHead className="pl-4">Sessão</TableHead>
                  <TableHead>Conector</TableHead>
                  <TableHead>Anomalia</TableHead>
                  <TableHead>Resultado</TableHead>
                  <TableHead className="text-right">Duração</TableHead>
                  <TableHead className="pr-4 text-right">Quando</TableHead>
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
                        {RESULT_LABELS[session.result]}
                      </StatusPill>
                    </TableCell>
                    <TableCell className="text-right font-semibold">
                      {`${session.durationMinutes} min`}
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
        <QueryFallback isError={overview.isError} what="o auto-reparo" />
      )}
    </main>
  )
}
