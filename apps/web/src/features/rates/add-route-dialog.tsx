import { Plus } from 'lucide-react'
import { Fragment, useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'

import { useAddRouteMargins } from '@/api/queries'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { formatPercent } from '@/lib/format'

const ORIGIN_PORTS = [
  'BEANR',
  'CNNSA',
  'CNQDG',
  'CNSHA',
  'CNSZX',
  'DEHAM',
  'DESTR',
  'INNSA',
  'KRPUS',
  'NLRTM',
  'PTLIS',
  'USMIA',
]
const DESTINATION_PORTS = ['BRITJ', 'BRNVT', 'BRPNG', 'BRRIG', 'BRSSZ', 'BRSUA', 'BRVIX']
const MARGIN_PATTERN = /^\d+([.,]\d+)?$/

type RouteRow = { origin: string; destination: string; margin: string }

const emptyRow = (): RouteRow => ({ origin: '', destination: '', margin: '' })

const selectClassName =
  'border-input bg-background h-9 w-full rounded-md border px-2 text-sm shadow-xs outline-none focus-visible:ring-2 focus-visible:ring-ring/50'

type AddRouteDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  floorPercent: string
}

export function AddRouteDialog({ open, onOpenChange, floorPercent }: AddRouteDialogProps) {
  const { t } = useTranslation()
  const [rows, setRows] = useState<RouteRow[]>([emptyRow()])
  const addRoutes = useAddRouteMargins()

  const completeRows = rows.filter((row) => row.origin && row.destination)
  const marginsValid = rows.every(
    (row) => row.margin.trim() === '' || MARGIN_PATTERN.test(row.margin.trim()),
  )

  function updateRow(index: number, patch: Partial<RouteRow>) {
    setRows((current) => current.map((row, i) => (i === index ? { ...row, ...patch } : row)))
  }

  function handleOpenChange(next: boolean) {
    onOpenChange(next)
    if (!next) {
      setRows([emptyRow()])
      addRoutes.reset()
    }
  }

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    addRoutes.mutate(
      completeRows.map((row) => ({
        route: { origin: row.origin, destination: row.destination },
        marginPercent: row.margin.trim() ? row.margin.trim().replace(',', '.') : null,
      })),
      { onSuccess: () => handleOpenChange(false) },
    )
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-xl">
        <form onSubmit={handleSubmit} className="space-y-4">
          <DialogHeader>
            <DialogTitle>{t('addRoute.title')}</DialogTitle>
            <DialogDescription>
              {t('addRoute.description', { percent: formatPercent(floorPercent) })}
            </DialogDescription>
          </DialogHeader>

          <div className="grid grid-cols-[1fr_1fr_6rem] gap-2 text-sm">
            <span className="text-muted-foreground text-xs">{t('addRoute.origin')}</span>
            <span className="text-muted-foreground text-xs">{t('addRoute.destination')}</span>
            <span className="text-muted-foreground text-xs">{t('addRoute.margin')}</span>
            {rows.map((row, index) => (
              <Fragment key={index}>
                <select
                  aria-label={t('addRoute.originOf', { n: index + 1 })}
                  value={row.origin}
                  onChange={(event) => updateRow(index, { origin: event.target.value })}
                  className={selectClassName}
                >
                  <option value="">{t('addRoute.selectPort')}</option>
                  {ORIGIN_PORTS.map((port) => (
                    <option key={port}>{port}</option>
                  ))}
                </select>
                <select
                  aria-label={t('addRoute.destinationOf', { n: index + 1 })}
                  value={row.destination}
                  onChange={(event) => updateRow(index, { destination: event.target.value })}
                  className={selectClassName}
                >
                  <option value="">{t('addRoute.selectPort')}</option>
                  {DESTINATION_PORTS.map((port) => (
                    <option key={port}>{port}</option>
                  ))}
                </select>
                <Input
                  aria-label={t('addRoute.marginOf', { n: index + 1 })}
                  inputMode="decimal"
                  placeholder={formatPercent(floorPercent)}
                  value={row.margin}
                  onChange={(event) => updateRow(index, { margin: event.target.value })}
                  className="text-right"
                />
              </Fragment>
            ))}
          </div>

          <button
            type="button"
            onClick={() => setRows((current) => [...current, emptyRow()])}
            className="text-primary inline-flex items-center gap-1 text-sm hover:underline"
          >
            <Plus className="size-4" />
            {t('addRoute.addAnother')}
          </button>

          {!marginsValid ? (
            <p role="alert" className="text-sm text-red-700">
              {t('addRoute.invalidMargin')}
            </p>
          ) : null}
          {addRoutes.isError ? (
            <p role="alert" className="text-sm text-red-700">
              {t('addRoute.saveError')}
            </p>
          ) : null}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => handleOpenChange(false)}>
              {t('common.cancel')}
            </Button>
            <Button
              type="submit"
              disabled={completeRows.length === 0 || !marginsValid || addRoutes.isPending}
            >
              {t('addRoute.save')}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
