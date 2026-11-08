import { Outlet } from 'react-router'

import { useInboxQuotes, useSession } from '@/api/queries'

import { AppSidebar } from './app-sidebar'

export function AppLayout() {
  const session = useSession()
  const inbox = useInboxQuotes()
  const counts = inbox.data?.statusCounts
  const pendingCount = counts ? counts.awaiting_review + counts.ready_to_send : undefined

  return (
    <div className="flex min-h-svh">
      <AppSidebar session={session.data} inboxPendingCount={pendingCount} />
      <Outlet />
    </div>
  )
}
