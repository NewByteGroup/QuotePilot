import { useTranslation } from 'react-i18next'
import { NavLink, Outlet } from 'react-router'

import { useSession } from '@/api/queries'
import { PageHeader } from '@/components/panel'
import { cn } from '@/lib/utils'

export function RatesLayout() {
  const { t } = useTranslation()
  const session = useSession()
  const tabs = [
    { label: t('rates.tabNetRates'), to: '/tarifas', end: true },
    { label: t('rates.tabRules'), to: '/tarifas/regras', end: false },
    { label: t('rates.tabIdentity'), to: '/tarifas/identidade', end: false },
  ]
  return (
    <main className="min-w-0 flex-1 space-y-5 p-6">
      <PageHeader
        title={t('rates.title')}
        subtitle={t('rates.subtitle', { org: session.data?.organization.name ?? '…' })}
      />
      <nav aria-label={t('rates.sections')} className="flex gap-6 border-b">
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
