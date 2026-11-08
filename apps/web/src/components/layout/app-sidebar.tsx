import { ChevronDown } from 'lucide-react'
import { NavLink, useLocation } from 'react-router'

import type { Schemas } from '@/api/client'
import { cn } from '@/lib/utils'

type NavItem = { label: string; to: string; badge?: number; active?: boolean }

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
        <NavLink
          key={item.label}
          to={item.to}
          aria-current={item.active ? 'page' : undefined}
          className={cn(
            'text-foreground/80 hover:bg-muted flex items-center justify-between rounded-md px-2 py-2 text-sm transition-colors',
            item.active && 'bg-accent text-accent-foreground hover:bg-accent font-medium',
          )}
        >
          {item.label}
          {item.badge ? <span className="text-xs font-semibold">{item.badge}</span> : null}
        </NavLink>
      ))}
    </div>
  )
}

function initials(name: string): string {
  return name
    .split(' ')
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part.charAt(0).toUpperCase())
    .join('')
}

export function AppSidebar({ session, inboxPendingCount }: AppSidebarProps) {
  const { pathname } = useLocation()
  const inboxActive = pathname === '/' || pathname.startsWith('/quotes')
  const organization = session?.organization
  const user = session?.user
  return (
    <aside className="bg-card sticky top-0 hidden h-svh w-60 shrink-0 flex-col border-r px-3 py-5 lg:flex">
      <div className="flex items-center gap-2 px-2">
        <img src="/logo.png" alt="" aria-hidden className="size-7" />
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

      <nav className="mt-3 flex-1" aria-label="Principal">
        <NavSection
          title="Empresa"
          items={[
            { label: 'Inbox de cotações', to: '/', badge: inboxPendingCount, active: inboxActive },
            { label: 'Tarifas & regras', to: '#' },
          ]}
        />
        <NavSection
          title="Plataforma"
          items={[
            { label: 'Conectores', to: '#' },
            { label: 'Auto-reparo', to: '#' },
          ]}
        />
      </nav>

      {user ? (
        <div className="flex items-center gap-3 border-t px-2 pt-4">
          <span className="bg-accent text-accent-foreground flex size-9 items-center justify-center rounded-full text-xs font-semibold">
            {initials(user.name)}
          </span>
          <span className="min-w-0 leading-tight">
            <span className="block truncate text-sm font-medium">{user.name}</span>
            <span className="text-muted-foreground block truncate text-xs">
              {user.role === 'admin' ? 'Admin' : 'Operador'} · {user.organizationCount} empresas
            </span>
          </span>
        </div>
      ) : null}
    </aside>
  )
}
