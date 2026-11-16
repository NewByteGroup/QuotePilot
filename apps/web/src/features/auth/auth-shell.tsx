import type { ReactNode } from 'react'

export function AuthShell({ title, children }: { title: string; children: ReactNode }) {
  return (
    <main className="bg-muted/40 flex min-h-svh items-center justify-center p-6">
      <div className="bg-card w-full max-w-sm space-y-5 rounded-xl border p-6 shadow-sm">
        <div className="flex items-center gap-2">
          <img src="/logo.png" alt="" aria-hidden className="size-7" />
          <span className="font-semibold">QuotePilot</span>
        </div>
        <h1 className="text-lg font-semibold">{title}</h1>
        {children}
      </div>
    </main>
  )
}
