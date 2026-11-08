import { Mail } from 'lucide-react'
import { Link, useNavigate } from 'react-router'

import type { Schemas } from '@/api/client'
import { Badge } from '@/components/ui/badge'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { cn } from '@/lib/utils'

import {
  EQUIPMENT_LABELS,
  formatDuration,
  formatMoney,
  formatReceivedAt,
  minutesUntil,
} from './format'
import { StatusBadge } from './status-badge'

const AUTO_APPROVE_CONFIDENCE = 95

function Dash() {
  return <span className="text-muted-foreground">—</span>
}

function Confidence({ percent }: { percent: number | null }) {
  if (percent === null) return <Dash />
  const high = percent >= AUTO_APPROVE_CONFIDENCE
  return (
    <div className="flex items-center justify-end gap-3">
      <span
        aria-hidden
        className={cn('h-1 w-9 rounded-full', high ? 'bg-green-700' : 'bg-orange-700')}
      />
      <span
        className={cn(
          'w-10 text-right text-xs font-medium',
          high ? 'text-green-700' : 'text-orange-700',
        )}
      >
        {percent}%
      </span>
    </div>
  )
}

type QuotesTableProps = {
  quotes: Schemas['InboxQuote'][]
  totalCount: number
  now: Date
}

export function QuotesTable({ quotes, totalCount, now }: QuotesTableProps) {
  const navigate = useNavigate()
  return (
    <div className="bg-card overflow-hidden rounded-lg border">
      <Table>
        <TableHeader>
          <TableRow className="text-xs hover:bg-transparent">
            <TableHead className="pl-4">Cliente / assunto</TableHead>
            <TableHead>Rota</TableHead>
            <TableHead>Modal</TableHead>
            <TableHead>Status</TableHead>
            <TableHead className="text-right">Valor estimado</TableHead>
            <TableHead className="text-right">Confiança</TableHead>
            <TableHead className="text-right">SLA</TableHead>
            <TableHead className="pr-4 text-right">Recebido</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {quotes.map((quote) => (
            <TableRow
              key={quote.id}
              data-sla-at-risk={quote.slaAtRisk || undefined}
              onClick={() => navigate(`/quotes/${quote.id}`)}
              className={cn(
                'h-14 cursor-pointer',
                quote.slaAtRisk && 'bg-red-50 hover:bg-red-50/80',
              )}
            >
              <TableCell className="pl-4">
                <div className="flex items-center gap-3">
                  <Mail className="text-muted-foreground size-4 shrink-0" aria-hidden />
                  <div className="min-w-0">
                    <Link
                      to={`/quotes/${quote.id}`}
                      onClick={(event) => event.stopPropagation()}
                      className="block truncate font-medium hover:underline"
                    >
                      {quote.customerName}
                    </Link>
                    <p className="text-muted-foreground truncate text-xs">{quote.subject}</p>
                  </div>
                </div>
              </TableCell>
              <TableCell>
                {quote.route.origin} → {quote.route.destination}
              </TableCell>
              <TableCell>
                <Badge variant="secondary" className="rounded-sm font-normal">
                  {EQUIPMENT_LABELS[quote.equipment]}
                </Badge>
              </TableCell>
              <TableCell>
                <StatusBadge status={quote.status} />
              </TableCell>
              <TableCell className="text-right font-semibold">
                {quote.estimatedTotal ? formatMoney(quote.estimatedTotal) : <Dash />}
              </TableCell>
              <TableCell className="text-right">
                <Confidence percent={quote.confidencePercent} />
              </TableCell>
              <TableCell
                className={cn('text-right font-semibold', quote.slaAtRisk && 'text-red-600')}
              >
                {quote.slaDueAt ? formatDuration(minutesUntil(quote.slaDueAt, now)) : <Dash />}
              </TableCell>
              <TableCell className="text-muted-foreground pr-4 text-right text-xs">
                {formatReceivedAt(quote.receivedAt, now)}
              </TableCell>
            </TableRow>
          ))}
          {quotes.length === 0 ? (
            <TableRow>
              <TableCell colSpan={8} className="text-muted-foreground py-10 text-center">
                Nenhuma cotação encontrada.
              </TableCell>
            </TableRow>
          ) : null}
        </TableBody>
      </Table>
      <p className="text-muted-foreground border-t px-4 py-3 text-xs">
        Mostrando {quotes.length} de {totalCount} cotações
      </p>
    </div>
  )
}
