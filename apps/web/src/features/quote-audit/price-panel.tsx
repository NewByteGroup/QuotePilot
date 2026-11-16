import type { Schemas } from '@/api/client'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { formatMoney } from '@/features/inbox/format'
import { formatPercent } from '@/lib/format'
import { cn } from '@/lib/utils'

const SOURCE_STYLES: Record<Schemas['PriceSource']['kind'], { chip: string; dot: string }> = {
  private_rate: { chip: 'bg-violet-50 text-violet-700', dot: 'bg-violet-500' },
  public_snapshot: { chip: 'bg-amber-50 text-amber-700', dot: 'bg-amber-500' },
}

function Dot({ inferred }: { inferred: boolean }) {
  return (
    <span
      aria-hidden
      className={cn(
        'mt-2 size-1.5 shrink-0 rounded-full',
        inferred ? 'bg-orange-500' : 'bg-green-600',
      )}
    />
  )
}

function Section({ section }: { section: Schemas['PriceSection'] }) {
  return (
    <section aria-label={section.title} className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-semibold">{section.title}</h3>
        {section.source ? (
          <span
            className={cn(
              'inline-flex items-center gap-1.5 rounded-sm px-1.5 py-0.5 text-xs',
              SOURCE_STYLES[section.source.kind].chip,
            )}
          >
            <span
              aria-hidden
              className={cn('size-1.5 rounded-full', SOURCE_STYLES[section.source.kind].dot)}
            />
            {section.source.label}
          </span>
        ) : null}
      </div>
      <ul className="space-y-1">
        {section.items.map((item) => (
          <li key={item.label} className="flex items-start justify-between gap-4 text-sm">
            <span className="flex gap-2">
              <Dot inferred={item.inferred} />
              <span className="leading-snug">
                {item.label}
                {item.detail ? (
                  <span className="text-muted-foreground block text-xs">{item.detail}</span>
                ) : null}
              </span>
            </span>
            <span className="shrink-0 font-medium tabular-nums">{formatMoney(item.amount)}</span>
          </li>
        ))}
      </ul>
      {section.note ? <p className="text-muted-foreground text-xs">{section.note}</p> : null}
    </section>
  )
}

type PricePanelProps = {
  pricing: Schemas['PriceComposition'] | null
  status: Schemas['QuoteStatus']
}

export function PricePanel({ pricing, status }: PricePanelProps) {
  const hasInferred = pricing?.sections.some((section) => section.items.some((i) => i.inferred))
  const pendingHint = 'Disponível quando o core-api expuser a ação no contrato.'
  return (
    <Card className="gap-0 rounded-lg py-0 shadow-none">
      <CardHeader className="flex flex-row items-center justify-between border-b px-4 py-4">
        <CardTitle className="text-base">Composição do preço</CardTitle>
        <Badge variant="secondary" className="rounded-sm font-normal">
          memória de cálculo auditável
        </Badge>
      </CardHeader>

      {pricing ? (
        <>
          <CardContent className="space-y-5 p-4">
            {pricing.sections.map((section) => (
              <Section key={section.title} section={section} />
            ))}

            <div className="space-y-1.5 border-t pt-3 text-sm">
              <div className="flex justify-between">
                <span className="text-muted-foreground">Custo total</span>
                <span className="font-semibold tabular-nums">{formatMoney(pricing.totalCost)}</span>
              </div>
              <div className="flex justify-between">
                <span className="text-muted-foreground">
                  {`Margem alvo (${formatPercent(pricing.marginPercent, 0)})`}
                </span>
                <span className="font-semibold text-green-700 tabular-nums">
                  {formatMoney(pricing.margin)}
                </span>
              </div>
            </div>

            <div className="flex items-center justify-between rounded-lg bg-violet-50 px-4 py-4 text-violet-700">
              <span className="font-medium">Total da proposta</span>
              <span className="text-2xl font-bold tabular-nums">
                {formatMoney(pricing.proposalTotal)}
              </span>
            </div>

            <div className="text-muted-foreground flex flex-wrap gap-5 text-xs">
              <span className="flex items-center gap-1.5">
                <span aria-hidden className="size-1.5 rounded-full bg-green-600" />
                {`${pricing.confidencePercent}% confiança`}
              </span>
              {hasInferred ? (
                <span className="flex items-center gap-1.5">
                  <span aria-hidden className="size-1.5 rounded-full bg-orange-500" />
                  inferido — revise antes de enviar
                </span>
              ) : null}
            </div>
          </CardContent>

          <div className="space-y-3 border-t p-4">
            <div className="flex flex-wrap justify-end gap-2">
              <Button variant="ghost" disabled title={pendingHint}>
                Reprovar
              </Button>
              <Button variant="outline" disabled title={pendingHint}>
                Editar valores
              </Button>
              <Button disabled title={pendingHint}>
                {status === 'sent' ? 'Enviado' : 'Aprovar e enviar (1-clique)'}
              </Button>
            </div>
            <p className="text-muted-foreground text-xs">
              O envio usa a identidade da empresa e responde na thread original do cliente (Resend).
            </p>
          </div>
        </>
      ) : (
        <CardContent className="text-muted-foreground p-4 text-sm">
          Cotação em processamento: a composição do preço aparece quando o agente terminar.
        </CardContent>
      )}
    </Card>
  )
}
