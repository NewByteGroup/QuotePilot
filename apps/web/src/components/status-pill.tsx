import type { ReactNode } from 'react'

import { cn } from '@/lib/utils'

export type PillTone = 'green' | 'amber' | 'red' | 'violet' | 'neutral' | 'outline'

const TONES: Record<PillTone, { pill: string; dot: string }> = {
  green: { pill: 'bg-green-50 text-green-700', dot: 'bg-green-600' },
  amber: { pill: 'bg-amber-50 text-amber-700', dot: 'bg-amber-500' },
  red: { pill: 'bg-red-50 text-red-700', dot: 'bg-red-600' },
  violet: { pill: 'bg-violet-50 text-violet-700', dot: 'bg-violet-500' },
  neutral: { pill: 'bg-muted text-foreground/80', dot: 'bg-neutral-400' },
  outline: { pill: 'bg-card text-muted-foreground border', dot: 'bg-neutral-400' },
}

export function StatusPill({ tone, children }: { tone: PillTone; children: ReactNode }) {
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-sm px-1.5 py-0.5 text-xs whitespace-nowrap',
        TONES[tone].pill,
      )}
    >
      <span aria-hidden className={cn('size-1.5 rounded-full', TONES[tone].dot)} />
      {children}
    </span>
  )
}
