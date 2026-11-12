import { ArrowUpRight } from 'lucide-react'
import type { Schemas } from '@/api/client'
import { StatusPill, type PillTone } from '@/components/status-pill'
import { formatTime } from '@/lib/format'
import { cn } from '@/lib/utils'

const TAG_TONES: Record<Schemas['RepairEventKind'], PillTone> = {
  anomaly: 'red',
  session_opened: 'neutral',
  tests_passed: 'neutral',
  pull_request_merged: 'neutral',
  reprocessed: 'green',
}

const DOT_STYLES: Record<Schemas['RepairEventKind'], string> = {
  anomaly: 'bg-red-600',
  session_opened: 'bg-neutral-400',
  tests_passed: 'bg-neutral-400',
  pull_request_merged: 'bg-neutral-400',
  reprocessed: 'bg-green-600',
}

type RepairTimelineProps = {
  timeline: Schemas['RepairTimeline']
  highlightAnomaly?: boolean
  showSessionLink?: boolean
}

export function RepairTimeline({
  timeline,
  highlightAnomaly = false,
  showSessionLink = false,
}: RepairTimelineProps) {
  return (
    <ol aria-label={`Linha do tempo · ${timeline.connectorName}`} className="px-4 py-3">
      {timeline.events.map((event, index) => (
        <li key={index} className="relative flex items-center gap-4 py-2 text-sm">
          {index < timeline.events.length - 1 ? (
            <span aria-hidden className="bg-border absolute top-1/2 left-[3px] h-full w-px" />
          ) : null}
          <span
            aria-hidden
            className={cn('relative size-2 shrink-0 rounded-full', DOT_STYLES[event.kind])}
          />
          <time dateTime={event.at} className="text-muted-foreground w-10 shrink-0 text-xs">
            {formatTime(event.at)}
          </time>
          <span
            className={cn('flex-1', highlightAnomaly && event.kind === 'anomaly' && 'text-red-600')}
          >
            {event.message}
          </span>
          <StatusPill tone={TAG_TONES[event.kind]}>{event.tag}</StatusPill>
          {showSessionLink && event.sessionUrl ? (
            <a
              href={event.sessionUrl}
              target="_blank"
              rel="noreferrer"
              className="text-primary inline-flex items-center gap-1 text-xs hover:underline"
            >
              Ver sessão
              <ArrowUpRight className="size-3" aria-hidden />
            </a>
          ) : null}
        </li>
      ))}
    </ol>
  )
}
