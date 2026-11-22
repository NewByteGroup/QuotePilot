import type { ReactNode } from 'react'

type AuthShellProps = {
  title: string
  description?: ReactNode
  children: ReactNode
}

export function AuthShell({ title, description, children }: AuthShellProps) {
  return (
    <main className="bg-muted/40 flex h-svh items-center justify-center overflow-y-auto p-6">
      <div className="w-full max-w-sm space-y-6">
        <div className="flex items-center justify-center gap-2.5">
          <img src="/logo.png" alt="" aria-hidden className="size-10" />
          <span className="text-2xl font-semibold tracking-tight">QuotePilot</span>
        </div>
        <div className="bg-card space-y-6 rounded-xl border p-8 shadow-sm">
          <div className="space-y-1.5">
            <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
            {description ? <p className="text-muted-foreground text-sm">{description}</p> : null}
          </div>
          {children}
        </div>
      </div>
    </main>
  )
}
