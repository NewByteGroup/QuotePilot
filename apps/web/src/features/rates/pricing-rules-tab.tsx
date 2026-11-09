import { useState } from 'react'
import { useTranslation } from 'react-i18next'

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
  const { t } = useTranslation()
  const rules = usePricingRules()
  const toggle = useUpdatePricingAutomation()
  const [dialogOpen, setDialogOpen] = useState(false)

  if (!rules.data) return <QueryFallback isError={rules.isError} what={t('pricingRules.what')} />
  const data = rules.data

  return (
    <div className="space-y-4">
      <Panel
        title={t('pricingRules.general')}
        action={
          <Button variant="outline" size="sm" disabled title={t('pricingRules.editPending')}>
            {t('common.edit')}
          </Button>
        }
      >
        <dl className="grid gap-4 p-4 text-sm sm:grid-cols-2">
          <Parameter
            label={t('pricingRules.targetMargin')}
            value={formatPercent(data.targetMarginPercent)}
          />
          <Parameter
            label={t('pricingRules.fxSpread')}
            value={t('pricingRules.fxSpreadValue', {
              percent: formatPercent(data.fxSpreadPercent, 2),
            })}
          />
          <Parameter label={t('pricingRules.dispatchFee')} value={formatMoney(data.dispatchFee)} />
          <Parameter
            label={t('pricingRules.floor')}
            value={formatPercent(data.marginFloorPercent)}
          />
        </dl>
      </Panel>

      <Panel
        title={t('pricingRules.routeMargins')}
        action={
          <Button size="sm" onClick={() => setDialogOpen(true)}>
            {t('pricingRules.addRoute')}
          </Button>
        }
      >
        <p className="text-muted-foreground px-4 pt-3 text-xs">
          {t('pricingRules.customizations', { percent: formatPercent(data.marginFloorPercent) })}
        </p>
        <Table>
          <TableHeader>
            <TableRow className="text-xs hover:bg-transparent">
              <TableHead className="pl-4">{t('pricingRules.route')}</TableHead>
              <TableHead className="text-right">{t('pricingRules.defaultMargin')}</TableHead>
              <TableHead className="text-right">{t('pricingRules.appliedMargin')}</TableHead>
              <TableHead className="pr-4">{t('pricingRules.status')}</TableHead>
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
                    <StatusPill tone="violet">{t('pricingRules.customized')}</StatusPill>
                  ) : (
                    <StatusPill tone="neutral">{t('pricingRules.default')}</StatusPill>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Panel>

      <Panel title={t('pricingRules.automations')}>
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
            {t('pricingRules.automationError')}
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
