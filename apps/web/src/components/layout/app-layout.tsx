import { Outlet } from 'react-router'

import { useCompanyIdentity, useInboxQuotes, useOpsConnectors, useSession } from '@/api/queries'

import { AppSidebar } from './app-sidebar'
import { MobileNav } from './mobile-nav'

export function AppLayout() {
  const session = useSession()
  const identity = useCompanyIdentity()
  const inbox = useInboxQuotes()
  const opsConnectors = useOpsConnectors()
  const counts = inbox.data?.statusCounts
  const pendingCount = counts ? counts.awaiting_review + counts.ready_to_send : undefined

  return (
    <div className="flex min-h-svh">
      <AppSidebar
        session={session.data}
        identity={identity.data}
        inboxPendingCount={pendingCount}
        showOpsConsole={opsConnectors.isSuccess}
      />
      <div className="flex min-w-0 flex-1 flex-col">
        <MobileNav
          session={session.data}
          identity={identity.data}
          inboxPendingCount={pendingCount}
          showOpsConsole={opsConnectors.isSuccess}
        />
        <Outlet />
      </div>
    </div>
  )
}
