import { useState } from 'react'
import { usePricingRules, useUpdatePricingAutomation } from '@/api/queries'
import { Panel, QueryFallback } from '@/components/panel'
import { StatusPill } from '@/components/status-pill'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { formatMoney } from '@/features/inbox/format'
import { formatPercent } from '@/lib/format'
import { cn } from '@/lib/utils'

import { AddRouteDialog } from './add-route-dialog'

function Parameter({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-muted-foreground text-xs">{label}</dt>
      <dd className="font-medium">{value}</dd>
    </div>
  )
}

export function PricingRulesTab() {
  const rules = usePricingRules()
  const toggle = useUpdatePricingAutomation()
  const [dialogOpen, setDialogOpen] = useState(false)

  if (!rules.data) return <QueryFallback isError={rules.isError} what="as regras de preço" />
  const data = rules.data

  return (
    <div className="space-y-4">
      <Panel
        title="Parâmetros gerais"
        action={
          <Button
            variant="outline"
            size="sm"
            disabled
            title="Disponível quando o core-api expuser a edição no contrato."
          >
            Editar
          </Button>
        }
      >
        <dl className="grid gap-4 p-4 text-sm sm:grid-cols-2">
          <Parameter label="Margem alvo" value={formatPercent(data.targetMarginPercent)} />
          <Parameter
            label="Spread cambial"
            value={`+${formatPercent(data.fxSpreadPercent, 2)} s/ dólar armador`}
          />
          <Parameter label="Taxa fixa de despacho" value={formatMoney(data.dispatchFee)} />
          <Parameter
            label="Piso de margem por rota"
            value={formatPercent(data.marginFloorPercent)}
          />
        </dl>
      </Panel>

      <Panel
        title="Margem por rota"
        action={
          <Button size="sm" onClick={() => setDialogOpen(true)}>
            Adicionar rota
          </Button>
        }
      >
        <p className="text-muted-foreground px-4 pt-3 text-xs">
          {`Personalizações sobre o piso global de ${formatPercent(data.marginFloorPercent)}`}
        </p>
        <Table>
          <TableHeader>
            <TableRow className="text-xs hover:bg-transparent">
              <TableHead className="pl-4">Rota</TableHead>
              <TableHead className="text-right">Margem padrão</TableHead>
              <TableHead className="text-right">Margem aplicada</TableHead>
              <TableHead className="pr-4">Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.routeMargins.map((item) => (
              <TableRow key={`${item.route.origin}-${item.route.destination}`}>
                <TableCell className="pl-4">
                  {item.route.origin} → {item.route.destination}
                </TableCell>
                <TableCell className="text-right">
                  {formatPercent(item.defaultMarginPercent)}
                </TableCell>
                <TableCell
                  className={cn('text-right font-semibold', item.customized && 'text-violet-700')}
                >
                  {formatPercent(item.appliedMarginPercent)}
                </TableCell>
                <TableCell className="pr-4">
                  {item.customized ? (
                    <StatusPill tone="violet">Personalizada</StatusPill>
                  ) : (
                    <StatusPill tone="neutral">Padrão</StatusPill>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Panel>

      <Panel title="Automações de preço">
        <ul className="divide-y">
          {data.automations.map((automation) => (
            <li
              key={automation.key}
              className="flex items-center justify-between px-4 py-2.5 text-sm"
            >
              {automation.label}
              <Switch
                aria-label={automation.label}
                checked={automation.enabled}
                disabled={toggle.isPending && toggle.variables?.key === automation.key}
                onCheckedChange={(enabled) => toggle.mutate({ key: automation.key, enabled })}
              />
            </li>
          ))}
        </ul>
        {toggle.isError ? (
          <p role="alert" className="border-t px-4 py-2 text-sm text-red-700">
            Não foi possível salvar a automação.
          </p>
        ) : null}
      </Panel>

      <AddRouteDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        floorPercent={data.marginFloorPercent}
      />
    </div>
  )
}
