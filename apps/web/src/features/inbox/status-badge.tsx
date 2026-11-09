import { useTranslation } from 'react-i18next'

import type { Schemas } from '@/api/client'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

const STATUS_STYLES: Record<Schemas['QuoteStatus'], { badge: string; dot: string }> = {
  ready_to_send: { badge: 'bg-green-50 text-green-700', dot: 'bg-green-600' },
  awaiting_review: { badge: 'bg-amber-50 text-amber-700', dot: 'bg-amber-500' },
  processing: { badge: 'bg-violet-50 text-violet-700', dot: 'bg-violet-500' },
  sent: { badge: 'border-border bg-card text-muted-foreground', dot: 'bg-neutral-400' },
}

export function StatusBadge({ status }: { status: Schemas['QuoteStatus'] }) {
  const { t } = useTranslation()
  return (
    <Badge
      className={cn('rounded-sm font-normal', STATUS_STYLES[status].badge)}
      variant={status === 'sent' ? 'outline' : 'default'}
    >
      <span aria-hidden className={cn('size-1.5 rounded-full', STATUS_STYLES[status].dot)} />
      {t(`quoteStatus.${status}`)}
    </Badge>
  )
}
