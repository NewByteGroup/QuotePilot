import { ChevronDown } from 'lucide-react'

import type { Schemas } from '@/api/client'
import { cn } from '@/lib/utils'

type NavItem = { label: string; badge?: number; active?: boolean }

type AppSidebarProps = {
  session?: Schemas['Session']
  inboxPendingCount?: number
}

function NavSection({ title, items }: { title: string; items: NavItem[] }) {
  return (
    <div className="space-y-1">
      <p className="text-muted-foreground px-2 pt-4 pb-1 text-[11px] font-semibold tracking-wider uppercase">
        {title}
      </p>
      {items.map((item) => (
        <a
          key={item.label}
          href="#"
          aria-current={item.active ? 'page' : undefined}
          className={cn(
            'text-foreground/80 hover:bg-muted flex items-center justify-between rounded-md px-2 py-2 text-sm transition-colors',
            item.active && 'bg-accent text-accent-foreground hover:bg-accent font-medium',
          )}
        >
          {item.label}
          {item.badge ? <span className="text-xs font-semibold">{item.badge}</span> : null}
        </a>
      ))}
    </div>
  )
}

export function AppSidebar({ session, inboxPendingCount }: AppSidebarProps) {
  const organization = session?.organization
  return (
    <aside className="bg-card hidden w-60 shrink-0 flex-col border-r px-3 py-5 lg:flex">
      <div className="flex items-center gap-2 px-2">
        <span
          aria-hidden
          className="size-6 rounded-full bg-[conic-gradient(from_180deg,#8b5cf6,#38bdf8,#a78bfa,#8b5cf6)] p-[5px]"
        >
          <span className="bg-card block size-full rounded-full" />
        </span>
        <span className="font-semibold">QuotePilot</span>
      </div>

      <button
        type="button"
        className="hover:bg-muted mt-5 flex w-full items-center gap-3 rounded-lg border px-3 py-2.5 text-left"
      >
        <span className="flex size-8 items-center justify-center rounded-md bg-neutral-900 text-sm font-semibold text-white">
          {organization?.name.charAt(0) ?? '·'}
        </span>
        <span className="min-w-0 flex-1 leading-tight">
          <span className="block truncate text-sm font-medium">{organization?.name ?? '—'}</span>
          <span className="text-muted-foreground block truncate text-xs">
            {organization?.planName ?? ''}
          </span>
        </span>
        <ChevronDown className="text-muted-foreground size-4" />
      </button>

      <nav className="mt-3" aria-label="Principal">
        <NavSection
          title="Empresa"
          items={[
            { label: 'Inbox de cotações', badge: inboxPendingCount, active: true },
            { label: 'Tarifas & regras' },
          ]}
        />
        <NavSection
          title="Plataforma"
          items={[{ label: 'Conectores' }, { label: 'Auto-reparo' }]}
        />
      </nav>
    </aside>
  )
}
