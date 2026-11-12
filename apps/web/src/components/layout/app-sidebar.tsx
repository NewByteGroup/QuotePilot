import { ChevronDown, Settings } from 'lucide-react'
import { useState } from 'react'
import { NavLink, useLocation } from 'react-router'

import type { Schemas } from '@/api/client'
import { initials } from '@/lib/format'
import { cn } from '@/lib/utils'

import { OrganizationMenu } from './organization-menu'
import { SettingsDialog } from './settings-dialog'

type NavItem = { label: string; to: string; badge?: number; active?: boolean }

type AppSidebarProps = {
  session?: Schemas['Session']
  identity?: Schemas['CompanyIdentity']
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
          key={item.to}
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

export function AppSidebar({ session, identity, inboxPendingCount }: AppSidebarProps) {
  const { pathname } = useLocation()
  const [settingsOpen, setSettingsOpen] = useState(false)
  const organization = session?.organization
  const user = session?.user
  const companyName = identity?.legalName ?? organization?.name
  const canSwitchOrganization = (session?.organizations.length ?? 0) > 1

  const organizationCard = (
    <button
      type="button"
      aria-label={canSwitchOrganization ? 'Trocar de empresa' : undefined}
      disabled={!canSwitchOrganization}
      className="enabled:hover:bg-muted mt-5 flex w-full items-center gap-3 rounded-lg border px-3 py-2.5 text-left disabled:cursor-default"
    >
      {identity?.letterheadLogoUrl ? (
        <img
          src={identity.letterheadLogoUrl}
          alt=""
          className="size-8 shrink-0 rounded-md border bg-white object-contain p-0.5"
        />
      ) : (
        <span
          className="flex size-8 shrink-0 items-center justify-center rounded-md text-sm font-semibold text-white"
          style={{ backgroundColor: identity?.primaryColor ?? '#171717' }}
        >
          {companyName?.charAt(0) ?? '·'}
        </span>
      )}
      <span className="min-w-0 flex-1 leading-tight">
        <span className="block truncate text-sm font-medium">{companyName ?? '—'}</span>
        <span className="text-muted-foreground block truncate text-xs">
          {organization?.planName ?? ''}
        </span>
      </span>
      {canSwitchOrganization ? <ChevronDown className="text-muted-foreground size-4" /> : null}
    </button>
  )

  return (
    <aside className="bg-card sticky top-0 hidden h-svh w-60 shrink-0 flex-col border-r px-3 py-5 lg:flex">
      <div className="flex items-center gap-2 px-2">
        <img src="/logo.png" alt="" aria-hidden className="size-7" />
        <span className="font-semibold">QuotePilot</span>
      </div>

      {session && canSwitchOrganization ? (
        <OrganizationMenu session={session}>{organizationCard}</OrganizationMenu>
      ) : (
        organizationCard
      )}

      <nav className="mt-3 flex-1" aria-label="Principal">
        <NavSection
          title="Empresa"
          items={[
            {
              label: 'Inbox de cotações',
              to: '/',
              badge: inboxPendingCount,
              active: pathname === '/' || pathname.startsWith('/quotes'),
            },
            { label: 'Tarifas & regras', to: '/tarifas', active: pathname.startsWith('/tarifas') },
          ]}
        />
        <NavSection
          title="Plataforma"
          items={[
            {
              label: 'Conectores',
              to: '/conectores',
              active: pathname.startsWith('/conectores'),
            },
            {
              label: 'Auto-reparo',
              to: '/auto-reparo',
              active: pathname.startsWith('/auto-reparo'),
            },
          ]}
        />
      </nav>

      {user ? (
        <div className="flex items-center gap-3 border-t px-2 pt-4">
          <span className="bg-accent text-accent-foreground flex size-9 items-center justify-center rounded-full text-xs font-semibold">
            {initials(user.name)}
          </span>
          <span className="min-w-0 flex-1 leading-tight">
            <span className="block truncate text-sm font-medium">{user.name}</span>
            <span className="text-muted-foreground block truncate text-xs">
              {user.role === 'admin' ? 'Admin' : 'Operador'} ·{' '}
              {`${user.organizationCount} empresas`}
            </span>
          </span>
          <button
            type="button"
            aria-label="Configurações"
            onClick={() => setSettingsOpen(true)}
            className="text-muted-foreground hover:bg-muted hover:text-foreground rounded-md p-1.5 transition-colors"
          >
            <Settings className="size-4" />
          </button>
          <SettingsDialog open={settingsOpen} onOpenChange={setSettingsOpen} user={user} />
        </div>
      ) : null}
    </aside>
  )
}
