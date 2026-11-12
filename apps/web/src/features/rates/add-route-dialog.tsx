import { Plus } from 'lucide-react'
import { Fragment, useState, type FormEvent } from 'react'
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
            <DialogTitle>Adicionar rota</DialogTitle>
            <DialogDescription>
              {`Defina origem, destino e margem. Margem vazia usa o piso global de ${formatPercent(floorPercent)}.`}
            </DialogDescription>
          </DialogHeader>

          <div className="grid grid-cols-[1fr_1fr_6rem] gap-2 text-sm">
            <span className="text-muted-foreground text-xs">Origem</span>
            <span className="text-muted-foreground text-xs">Destino</span>
            <span className="text-muted-foreground text-xs">Margem</span>
            {rows.map((row, index) => (
              <Fragment key={index}>
                <select
                  aria-label={`Origem da rota ${index + 1}`}
                  value={row.origin}
                  onChange={(event) => updateRow(index, { origin: event.target.value })}
                  className={selectClassName}
                >
                  <option value="">Selecionar porto…</option>
                  {ORIGIN_PORTS.map((port) => (
                    <option key={port}>{port}</option>
                  ))}
                </select>
                <select
                  aria-label={`Destino da rota ${index + 1}`}
                  value={row.destination}
                  onChange={(event) => updateRow(index, { destination: event.target.value })}
                  className={selectClassName}
                >
                  <option value="">Selecionar porto…</option>
                  {DESTINATION_PORTS.map((port) => (
                    <option key={port}>{port}</option>
                  ))}
                </select>
                <Input
                  aria-label={`Margem da rota ${index + 1}`}
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
            Adicionar outra rota
          </button>

          {!marginsValid ? (
            <p role="alert" className="text-sm text-red-700">
              Margem inválida: use um número, por exemplo 10,5.
            </p>
          ) : null}
          {addRoutes.isError ? (
            <p role="alert" className="text-sm text-red-700">
              Não foi possível salvar as rotas.
            </p>
          ) : null}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => handleOpenChange(false)}>
              Cancelar
            </Button>
            <Button
              type="submit"
              disabled={completeRows.length === 0 || !marginsValid || addRoutes.isPending}
            >
              Salvar rotas
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
