import { Check } from 'lucide-react'
import type { ReactNode } from 'react'
import { useNavigate } from 'react-router'

import type { Schemas } from '@/api/client'
import { useSwitchOrganization } from '@/api/queries'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'

type OrganizationMenuProps = {
  session: Schemas['Session']
  children: ReactNode
}

// Opens under the company card; picking another organization reloads every screen for it.
export function OrganizationMenu({ session, children }: OrganizationMenuProps) {
  const navigate = useNavigate()
  const switchOrganization = useSwitchOrganization({ onSwitched: () => void navigate('/') })

  function select(organization: Schemas['SessionOrganization']) {
    if (organization.id === session.organization.id) return
    switchOrganization.mutate({ organizationId: organization.id })
  }

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>{children}</DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="w-(--radix-dropdown-menu-trigger-width)">
          <DropdownMenuLabel>Suas empresas</DropdownMenuLabel>
          {session.organizations.map((organization) => {
            const selected = organization.id === session.organization.id
            return (
              <DropdownMenuItem
                key={organization.id}
                role="menuitemradio"
                aria-checked={selected}
                disabled={switchOrganization.isPending}
                onSelect={() => select(organization)}
                className="justify-between"
              >
                <span className="min-w-0">
                  <span className="block truncate font-medium">{organization.name}</span>
                  <span className="text-muted-foreground block truncate text-xs">
                    {organization.planName}
                  </span>
                </span>
                {selected ? <Check aria-hidden className="size-4 shrink-0" /> : null}
              </DropdownMenuItem>
            )
          })}
        </DropdownMenuContent>
      </DropdownMenu>
      {switchOrganization.isError ? (
        <p role="alert" className="px-2 pt-2 text-xs text-red-700">
          Não foi possível trocar de empresa. Tente de novo.
        </p>
      ) : null}
    </>
  )
}
