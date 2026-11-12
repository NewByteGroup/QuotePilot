import { Menu } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'

import { Dialog, DialogContent, DialogTitle, DialogTrigger } from '@/components/ui/dialog'

import { SidebarContent, type AppSidebarProps } from './app-sidebar'

export function MobileNav({ session, identity, inboxPendingCount }: AppSidebarProps) {
  const [open, setOpen] = useState(false)
  const organizationId = session?.organization.id
  const previousOrganizationId = useRef(organizationId)

  // A company switch swaps the session under the drawer; close it once the new one lands.
  useEffect(() => {
    const switched =
      previousOrganizationId.current !== undefined &&
      previousOrganizationId.current !== organizationId
    previousOrganizationId.current = organizationId
    if (switched) setOpen(false)
  }, [organizationId])

  return (
    <div className="bg-card sticky top-0 z-40 flex items-center justify-between border-b px-4 py-3 lg:hidden">
      <div className="flex items-center gap-2">
        <img src="/logo.png" alt="" aria-hidden className="size-6" />
        <span className="text-sm font-semibold">QuotePilot</span>
      </div>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogTrigger asChild>
          <button
            type="button"
            aria-label="Abrir menu"
            className="text-muted-foreground hover:bg-muted hover:text-foreground rounded-md p-1.5 transition-colors"
          >
            <Menu aria-hidden className="size-5" />
          </button>
        </DialogTrigger>
        <DialogContent
          aria-describedby={undefined}
          className="bg-card data-[state=closed]:slide-out-to-left data-[state=open]:slide-in-from-left data-[state=closed]:animate-out data-[state=open]:animate-in top-0 left-0 flex h-svh w-72 translate-x-0 translate-y-0 flex-col gap-0 overflow-y-auto rounded-none p-0 duration-300"
        >
          <DialogTitle className="sr-only">Menu</DialogTitle>
          <div className="flex min-h-0 flex-1 flex-col px-3 py-5">
            <SidebarContent
              session={session}
              identity={identity}
              inboxPendingCount={inboxPendingCount}
              onNavigate={() => setOpen(false)}
            />
          </div>
        </DialogContent>
      </Dialog>
    </div>
  )
}
