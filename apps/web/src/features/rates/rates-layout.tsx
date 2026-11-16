import { NavLink, Outlet } from 'react-router'

import { useSession } from '@/api/queries'
import { PageHeader } from '@/components/panel'
import { cn } from '@/lib/utils'

export function RatesLayout() {
  const session = useSession()
  const tabs = [
    { label: 'Net rates', to: '/tarifas', end: true },
    { label: 'Regras de preço', to: '/tarifas/regras', end: false },
    { label: 'Identidade da empresa', to: '/tarifas/identidade', end: false },
  ]
  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <PageHeader
        title="Tarifas & regras"
        subtitle={`Dados privados da ${session.data?.organization.name ?? '…'} · isolados por RLS (organization_id)`}
      />
      <nav aria-label="Seções de tarifas e regras" className="flex gap-6 border-b">
        {tabs.map((tab) => (
          <NavLink
            key={tab.to}
            to={tab.to}
            end={tab.end}
            className={({ isActive }) =>
              cn(
                '-mb-px border-b-2 pb-2 text-sm transition-colors',
                isActive
                  ? 'border-primary text-primary font-medium'
                  : 'text-muted-foreground hover:text-foreground border-transparent',
              )
            }
          >
            {tab.label}
          </NavLink>
        ))}
      </nav>
      <Outlet />
    </main>
  )
}
