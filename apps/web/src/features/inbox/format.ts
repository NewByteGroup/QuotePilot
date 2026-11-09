import type { Schemas } from '@/api/client'
import i18n, { currentLocale } from '@/i18n'

const MINUTE_MS = 60_000

export function statusLabel(status: Schemas['QuoteStatus']): string {
  return i18n.t(`quoteStatus.${status}`)
}

export function equipmentLabel(equipment: Schemas['Equipment']): string {
  return i18n.t(`equipment.${equipment}`)
}

export function formatMoney(money: Schemas['Money']): string {
  return new Intl.NumberFormat(currentLocale(), {
    style: 'currency',
    currency: money.currency,
  }).format(Number(money.amount))
}

export function minutesUntil(iso: string, now: Date): number {
  return Math.max(0, Math.round((new Date(iso).getTime() - now.getTime()) / MINUTE_MS))
}

export function formatDuration(totalMinutes: number): string {
  const hours = Math.floor(totalMinutes / 60)
  const minutes = totalMinutes % 60
  return `${hours}h ${String(minutes).padStart(2, '0')}m`
}

function startOfDay(date: Date): number {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime()
}

export function formatReceivedAt(iso: string, now: Date): string {
  const date = new Date(iso)
  const locale = currentLocale()
  const time = date.toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit' })
  const daysAgo = Math.round((startOfDay(now) - startOfDay(date)) / (24 * 60 * MINUTE_MS))
  if (daysAgo === 0) return i18n.t('time.todayAt', { time })
  if (daysAgo === 1) return i18n.t('time.yesterdayAt', { time })
  const day = date.toLocaleDateString(locale, { day: '2-digit', month: '2-digit' })
  return i18n.t('time.dateAt', { date: day, time })
}

export function formatRelative(iso: string, now: Date): string {
  const minutes = Math.max(0, Math.round((now.getTime() - new Date(iso).getTime()) / MINUTE_MS))
  if (minutes < 1) return i18n.t('time.now')
  if (minutes < 60) return i18n.t('time.minutesAgo', { count: minutes })
  return i18n.t('time.hoursAgo', { count: Math.floor(minutes / 60) })
}
