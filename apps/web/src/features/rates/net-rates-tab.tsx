import type { TFunction } from 'i18next'
import { useTranslation } from 'react-i18next'

import type { Schemas } from '@/api/client'
import { useNetRateTables } from '@/api/queries'
import { Panel, QueryFallback } from '@/components/panel'
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
import { formatTime } from '@/lib/format'

const DAY_MS = 86_400_000

const STATUS_TONES: Record<Schemas['NetRateTableStatus'], PillTone> = {
  active: 'green',
  expiring: 'amber',
  expired: 'red',
  processing: 'violet',
}

function parseDate(isoDate: string): Date {
  const [year, month, day] = isoDate.split('-').map(Number)
  return new Date(year, month - 1, day)
}

function daysUntil(isoDate: string, now: Date): number {
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  return Math.max(0, Math.round((parseDate(isoDate).getTime() - today.getTime()) / DAY_MS))
}

function statusLabel(table: Schemas['NetRateTable'], now: Date, t: TFunction): string {
  switch (table.status) {
    case 'active':
      return t('netRates.active')
    case 'expired':
      return t('netRates.expired')
    case 'processing':
      return t('netRates.processing', { percent: table.processingPercent ?? 0 })
    case 'expiring':
      return t('netRates.expiring', { count: daysUntil(table.validTo, now) })
  }
}

function validityRange(table: Schemas['NetRateTable']): string {
  const from = parseDate(table.validFrom)
  const to = parseDate(table.validTo)
  const day = (date: Date) => String(date.getDate()).padStart(2, '0')
  const month = (date: Date) => String(date.getMonth() + 1).padStart(2, '0')
  if (from.getMonth() === to.getMonth()) return `${day(from)}–${day(to)}/${month(to)}`
  return `${day(from)}/${month(from)}–${day(to)}/${month(to)}`
}

export function NetRatesTab() {
  const { t } = useTranslation()
  const tables = useNetRateTables()
  const now = new Date()
  const uploadPending = t('netRates.uploadPending')

  return (
    <Panel
      title={t('netRates.title')}
      action={
        <div className="flex gap-2">
          <Button variant="outline" size="sm" disabled title={uploadPending}>
            {t('netRates.newFolder')}
          </Button>
          <Button size="sm" disabled title={uploadPending}>
            {t('netRates.upload')}
          </Button>
        </div>
      }
    >
      {tables.data ? (
        <>
          <Table>
            <TableHeader>
              <TableRow className="text-xs hover:bg-transparent">
                <TableHead className="pl-4">{t('netRates.carrier')}</TableHead>
                <TableHead>{t('netRates.route')}</TableHead>
                <TableHead>{t('netRates.validity')}</TableHead>
                <TableHead>{t('netRates.file')}</TableHead>
                <TableHead className="pr-4">{t('netRates.status')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tables.data.items.map((table) => (
                <TableRow key={table.id} className="h-14">
                  <TableCell className="pl-4 font-medium">{table.carrier}</TableCell>
                  <TableCell>{table.lane}</TableCell>
                  <TableCell>
                    <p className="text-xs font-semibold">{table.weekLabel}</p>
                    <p className="text-muted-foreground text-xs">
                      {table.status === 'processing'
                        ? t('netRates.uploadedAt', { time: formatTime(table.uploadedAt) })
                        : validityRange(table)}
                    </p>
                  </TableCell>
                  <TableCell className="text-xs">{table.fileName}</TableCell>
                  <TableCell className="pr-4">
                    <StatusPill tone={STATUS_TONES[table.status]}>
                      {statusLabel(table, now, t)}
                    </StatusPill>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {tables.data.staleQuoteCount > 0 ? (
            <p className="flex items-center gap-2 border-t px-4 py-3 text-sm text-red-600">
              <span aria-hidden className="size-1.5 rounded-full bg-red-600" />
              {t('netRates.stale', { count: tables.data.staleQuoteCount })}
            </p>
          ) : null}
        </>
      ) : (
        <div className="p-4">
          <QueryFallback isError={tables.isError} what={t('netRates.what')} />
        </div>
      )}
    </Panel>
  )
}
