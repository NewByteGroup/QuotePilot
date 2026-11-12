import { useTranslation } from 'react-i18next'

import type { Schemas } from '@/api/client'
import { cn } from '@/lib/utils'

export type StatusFilter = Schemas['QuoteStatus'] | 'all'

const ORDER: StatusFilter[] = ['all', 'processing', 'awaiting_review', 'ready_to_send', 'sent']

type StatusFiltersProps = {
  counts: Schemas['InboxStatusCounts']
  value: StatusFilter
  onChange: (value: StatusFilter) => void
}

export function StatusFilters({ counts, value, onChange }: StatusFiltersProps) {
  const { t } = useTranslation()
  return (
    <div role="group" aria-label={t('inbox.filterLabel')} className="flex flex-wrap gap-2">
      {ORDER.map((status) => (
        <button
          key={status}
          type="button"
          aria-pressed={value === status}
          onClick={() => onChange(status)}
          className={cn(
            'bg-card hover:bg-muted rounded-md border px-3 py-1.5 text-xs transition-colors',
            value === status && 'border-primary bg-accent text-accent-foreground hover:bg-accent',
          )}
        >
          {status === 'all' ? t('inbox.all') : t(`quoteStatus.${status}`)} · {counts[status]}
        </button>
      ))}
    </div>
  )
}
