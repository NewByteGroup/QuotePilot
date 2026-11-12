import { FileText } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import type { Schemas } from '@/api/client'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { formatReceivedAt } from '@/features/inbox/format'
import { cn } from '@/lib/utils'

const HIGHLIGHT_STYLES: Record<Schemas['EmailSegment']['highlight'], string> = {
  none: '',
  extracted: 'rounded bg-violet-50 px-1 py-0.5 text-violet-700',
  inferred: 'rounded bg-orange-50 px-1 py-0.5 text-orange-700',
}

function SectionLabel({ children }: { children: string }) {
  return (
    <p className="text-muted-foreground text-[11px] font-semibold tracking-wider uppercase">
      {children}
    </p>
  )
}

type EmailPanelProps = {
  email: Schemas['QuoteEmail']
  extraction: Schemas['ExtractedField'][]
  now: Date
}

export function EmailPanel({ email, extraction, now }: EmailPanelProps) {
  const { t } = useTranslation()
  return (
    <Card className="gap-0 rounded-lg py-0 shadow-none">
      <CardHeader className="flex flex-row items-center justify-between border-b px-4 py-4">
        <CardTitle className="text-base">{t('email.title')}</CardTitle>
        <Badge variant="secondary" className="rounded-sm font-normal">
          {t('email.thread')}
        </Badge>
      </CardHeader>
      <CardContent className="space-y-5 p-4">
        <dl className="grid grid-cols-[3.5rem_1fr] gap-y-1 border-b pb-4 text-sm">
          <dt className="text-muted-foreground text-xs leading-5">{t('email.from')}</dt>
          <dd>{email.from}</dd>
          <dt className="text-muted-foreground text-xs leading-5">{t('email.to')}</dt>
          <dd>{email.to}</dd>
          <dt className="text-muted-foreground text-xs leading-5">{t('email.date')}</dt>
          <dd>{formatReceivedAt(email.receivedAt, now)}</dd>
        </dl>

        <div className="space-y-2.5 text-sm leading-7">
          {email.paragraphs.map((segments, index) => (
            <p key={index}>
              {segments.map((segment, segmentIndex) => (
                <span key={segmentIndex} className={cn(HIGHLIGHT_STYLES[segment.highlight])}>
                  {segment.text}
                </span>
              ))}
            </p>
          ))}
        </div>

        {email.attachments.length > 0 ? (
          <div className="space-y-2">
            <SectionLabel>{t('email.attachments')}</SectionLabel>
            {email.attachments.map((attachment) => (
              <div
                key={attachment.fileName}
                className="flex items-center gap-3 rounded-md border px-3 py-2"
              >
                <FileText className="size-4 text-red-600" aria-hidden />
                <div className="leading-tight">
                  <p className="text-sm">{attachment.fileName}</p>
                  <p className="text-muted-foreground text-xs">
                    {Math.round(attachment.sizeBytes / 1024)} KB
                  </p>
                </div>
              </div>
            ))}
          </div>
        ) : null}

        <div className="space-y-2">
          <SectionLabel>{t('email.extraction')}</SectionLabel>
          <ul aria-label={t('email.extraction')} className="divide-y rounded-md border text-sm">
            {extraction.map((field) => (
              <li key={field.label} className="flex items-center justify-between px-3 py-2">
                <span className="flex items-center gap-2">
                  <span
                    aria-hidden
                    className={cn(
                      'size-1.5 rounded-full',
                      field.inferred ? 'bg-orange-500' : 'bg-green-600',
                    )}
                  />
                  {field.label}
                </span>
                <span className={cn('font-medium', field.inferred && 'text-orange-700')}>
                  {field.value}
                </span>
              </li>
            ))}
          </ul>
        </div>
      </CardContent>
    </Card>
  )
}
