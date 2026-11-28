import { useState } from 'react'

import type { CoreSchemas } from '@/api/client'
import {
  OPS_QUARANTINE_PAGE_SIZE,
  useOpsConnectors,
  useOpsHealingSessions,
  useOpsQuarantine,
} from '@/api/queries'
import { PageHeader, Panel, QueryFallback, StatCard } from '@/components/panel'
import { StatusPill, type PillTone } from '@/components/status-pill'
import { Button } from '@/components/ui/button'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { formatReceivedAt } from '@/features/inbox/format'
import { formatDecimal } from '@/lib/format'

const CONNECTOR_TONES: Record<CoreSchemas['OpsConnectorStatus'], PillTone> = {
  HEALTHY: 'green',
  DEGRADED: 'amber',
  HEALING: 'violet',
}

const CONNECTOR_LABELS: Record<CoreSchemas['OpsConnectorStatus'], string> = {
  HEALTHY: 'saudável',
  DEGRADED: 'degradado',
  HEALING: 'em reparo',
}

const HEALING_TONES: Record<CoreSchemas['OpsHealingStatus'], PillTone> = {
  OPEN: 'violet',
  PR_READY: 'amber',
  MERGED: 'green',
  FAILED: 'red',
}

const HEALING_LABELS: Record<CoreSchemas['OpsHealingStatus'], string> = {
  OPEN: 'sessão aberta',
  PR_READY: 'PR pronto',
  MERGED: 'mergeado',
  FAILED: 'falhou',
}

const QUARANTINE_LABELS: Record<CoreSchemas['OpsQuarantineReason'], string> = {
  no_match: 'sem correspondência',
  multi_match: 'múltiplas empresas',
}

function formatConfidence(score: string | null): string {
  return score ? `${formatDecimal(Number(score) * 100, 1)}%` : '—'
}

function hostOf(url: string): string {
  try {
    return new URL(url).host
  } catch {
    return url
  }
}

function externalLink(url: string | null, label: string) {
  if (!url) return <span className="text-muted-foreground">—</span>
  return (
    <a
      href={url}
      target="_blank"
      rel="noreferrer"
      className="text-primary text-sm font-medium hover:underline"
    >
      {label}
    </a>
  )
}

export function OpsConsolePage() {
  const [quarantinePage, setQuarantinePage] = useState(0)
  const quarantine = useOpsQuarantine(quarantinePage)
  const connectors = useOpsConnectors()
  const healing = useOpsHealingSessions()
  const now = new Date()

  const connectorItems = connectors.data?.items ?? []
  const healingItems = healing.data?.items ?? []
  const connectorNameById = new Map(connectorItems.map((connector) => [connector.id, connector]))
  const unhealthyCount = connectorItems.filter((connector) => connector.status !== 'HEALTHY').length
  const openHealingCount = healingItems.filter((session) =>
    ['OPEN', 'PR_READY'].includes(session.status),
  ).length
  const quarantineTotal = quarantine.data?.total
  const totalPages = quarantineTotal
    ? Math.max(1, Math.ceil(quarantineTotal / OPS_QUARANTINE_PAGE_SIZE))
    : 1

  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <PageHeader
        title="Ops Console"
        subtitle="Superfície interna da plataforma · acesso restrito a platform admins"
      />

      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard
          label="E-mails em quarentena"
          value={quarantineTotal?.toLocaleString('pt-BR') ?? '—'}
          hint="inbound sem resolução única de tenant"
        />
        <StatCard
          label="Conectores registrados"
          value={connectors.data ? String(connectorItems.length) : '—'}
          hint="fontes públicas monitoradas"
        />
        <StatCard
          label="Conectores em atenção"
          value={connectors.data ? String(unhealthyCount) : '—'}
          hint="degradados ou em reparo"
        />
        <StatCard
          label="Reparos ativos"
          value={healing.data ? String(openHealingCount) : '—'}
          hint="sessões abertas ou com PR pronto"
        />
      </div>

      <Panel
        title="Quarentena de entrada"
        description="Mensagens do Resend sem tenant único; nunca adivinhar o destino"
        badge={
          quarantineTotal !== undefined ? (
            <span className="bg-muted rounded-sm px-1.5 py-0.5 text-xs">
              {`${quarantineTotal} total`}
            </span>
          ) : undefined
        }
        action={
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={quarantinePage === 0 || quarantine.isFetching}
              onClick={() => setQuarantinePage((page) => Math.max(0, page - 1))}
            >
              Anterior
            </Button>
            <span className="text-muted-foreground text-xs whitespace-nowrap">
              {`${quarantinePage + 1}/${totalPages}`}
            </span>
            <Button
              variant="outline"
              size="sm"
              disabled={quarantinePage + 1 >= totalPages || quarantine.isFetching}
              onClick={() => setQuarantinePage((page) => page + 1)}
            >
              Próxima
            </Button>
          </div>
        }
      >
        {quarantine.data ? (
          <Table>
            <TableHeader>
              <TableRow className="text-xs hover:bg-transparent">
                <TableHead className="pl-4">Assunto</TableHead>
                <TableHead>Remetente</TableHead>
                <TableHead>Destinatários</TableHead>
                <TableHead>Motivo</TableHead>
                <TableHead className="pr-4 text-right">Recebido</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {quarantine.data.items.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5} className="text-muted-foreground py-6 text-center">
                    Nenhum e-mail em quarentena.
                  </TableCell>
                </TableRow>
              ) : (
                quarantine.data.items.map((item) => (
                  <TableRow key={item.id}>
                    <TableCell className="max-w-64 pl-4">
                      <p className="truncate font-medium">{item.subject}</p>
                      <p className="text-muted-foreground truncate text-xs">
                        {item.resendMessageId}
                      </p>
                    </TableCell>
                    <TableCell className="max-w-44 truncate">{item.fromAddr}</TableCell>
                    <TableCell className="max-w-56">
                      <p className="truncate">{item.recipients.join(', ')}</p>
                      {item.matchedOrgIds.length > 0 ? (
                        <p className="text-muted-foreground text-xs">
                          {`${item.matchedOrgIds.length} empresas candidatas`}
                        </p>
                      ) : null}
                    </TableCell>
                    <TableCell>
                      <StatusPill tone={item.reason === 'multi_match' ? 'amber' : 'red'}>
                        {QUARANTINE_LABELS[item.reason]}
                      </StatusPill>
                    </TableCell>
                    <TableCell className="text-muted-foreground pr-4 text-right text-xs whitespace-nowrap">
                      {formatReceivedAt(item.receivedAt, now)}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        ) : (
          <div className="p-4">
            <QueryFallback isError={quarantine.isError} what="a quarentena" />
          </div>
        )}
      </Panel>

      <div className="grid items-start gap-4 xl:grid-cols-2">
        <Panel title="Conectores" description="Saúde dos parsers de fontes públicas">
          {connectors.data ? (
            <Table>
              <TableHeader>
                <TableRow className="text-xs hover:bg-transparent">
                  <TableHead className="pl-4">Conector</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Confiança</TableHead>
                  <TableHead className="pr-4 text-right">Última coleta</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {connectorItems.map((connector) => (
                  <TableRow key={connector.id}>
                    <TableCell className="pl-4">
                      <p className="font-medium">{connector.displayName}</p>
                      <a
                        href={connector.targetUrl}
                        target="_blank"
                        rel="noreferrer"
                        className="text-muted-foreground hover:text-foreground text-xs hover:underline"
                      >
                        {`${connector.slug} · ${hostOf(connector.targetUrl)}`}
                      </a>
                    </TableCell>
                    <TableCell>
                      <StatusPill tone={CONNECTOR_TONES[connector.status]}>
                        {CONNECTOR_LABELS[connector.status]}
                      </StatusPill>
                    </TableCell>
                    <TableCell className="text-right font-semibold">
                      {formatConfidence(connector.confidenceScore)}
                    </TableCell>
                    <TableCell className="text-muted-foreground pr-4 text-right text-xs whitespace-nowrap">
                      {connector.lastScrapedAt
                        ? formatReceivedAt(connector.lastScrapedAt, now)
                        : '—'}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : (
            <div className="p-4">
              <QueryFallback isError={connectors.isError} what="os conectores de ops" />
            </div>
          )}
        </Panel>

        <Panel title="Healing · Devin" description="Feed de sessões de auto-reparo">
          {healing.data ? (
            <Table>
              <TableHeader>
                <TableRow className="text-xs hover:bg-transparent">
                  <TableHead className="pl-4">Conector</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Gate</TableHead>
                  <TableHead>Links</TableHead>
                  <TableHead className="pr-4 text-right">Criada</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {healingItems.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={5} className="text-muted-foreground py-6 text-center">
                      Nenhuma sessão de reparo registrada.
                    </TableCell>
                  </TableRow>
                ) : (
                  healingItems.map((session) => {
                    const connector = connectorNameById.get(session.connectorId)
                    return (
                      <TableRow key={session.id}>
                        <TableCell className="pl-4">
                          <p className="font-medium">
                            {connector?.displayName ?? 'conector removido'}
                          </p>
                          <p className="text-muted-foreground text-xs">
                            {connector?.slug ?? session.connectorId}
                          </p>
                        </TableCell>
                        <TableCell>
                          <StatusPill tone={HEALING_TONES[session.status]}>
                            {HEALING_LABELS[session.status]}
                          </StatusPill>
                        </TableCell>
                        <TableCell className="text-xs">
                          {session.pytestPassed === null
                            ? '—'
                            : session.pytestPassed
                              ? 'pytest ok'
                              : 'pytest falhou'}
                        </TableCell>
                        <TableCell>
                          <div className="flex gap-3">
                            {externalLink(session.devinSessionUrl, 'Devin')}
                            {externalLink(session.githubPrUrl, 'PR')}
                          </div>
                        </TableCell>
                        <TableCell className="text-muted-foreground pr-4 text-right text-xs whitespace-nowrap">
                          {formatReceivedAt(session.createdAt, now)}
                        </TableCell>
                      </TableRow>
                    )
                  })
                )}
              </TableBody>
            </Table>
          ) : (
            <div className="p-4">
              <QueryFallback isError={healing.isError} what="o feed de healing" />
            </div>
          )}
        </Panel>
      </div>
    </main>
  )
}
