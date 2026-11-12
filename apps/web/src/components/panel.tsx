import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

type PanelProps = {
  title: string
  description?: string
  badge?: ReactNode
  action?: ReactNode
  className?: string
  children: ReactNode
}

export function Panel({ title, description, badge, action, className, children }: PanelProps) {
  return (
    <section aria-label={title} className={cn('bg-card rounded-lg border', className)}>
      <header className="flex items-center justify-between gap-4 border-b px-4 py-3">
        <div>
          <div className="flex items-center gap-3">
            <h2 className="font-semibold">{title}</h2>
            {badge}
          </div>
          {description ? <p className="text-muted-foreground text-xs">{description}</p> : null}
        </div>
        {action}
      </header>
      {children}
    </section>
  )
}

export function StatCard({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="bg-card rounded-lg border p-4">
      <p className="text-muted-foreground text-xs">{label}</p>
      <p className="mt-1 text-2xl font-bold">{value}</p>
      <p className="text-muted-foreground mt-1 text-xs">{hint}</p>
    </div>
  )
}

export function QueryFallback({ isError, what }: { isError: boolean; what: string }) {
  if (isError) {
    return (
      <p
        role="alert"
        className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700"
      >
        {`Não foi possível carregar ${what}.`}
      </p>
    )
  }
  return <p className="text-muted-foreground text-sm">{`Carregando ${what}…`}</p>
}

export function PageHeader({ title, subtitle }: { title: string; subtitle: ReactNode }) {
  return (
    <header>
      <h1 className="text-2xl font-bold tracking-tight">{title}</h1>
      <p className="text-muted-foreground text-sm">{subtitle}</p>
    </header>
  )
}
