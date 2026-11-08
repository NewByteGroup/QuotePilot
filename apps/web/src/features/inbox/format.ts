import type { Schemas } from '@/api/client'

const MINUTE_MS = 60_000

export const STATUS_LABELS: Record<Schemas['QuoteStatus'], string> = {
  processing: 'Processando',
  awaiting_review: 'Aguardando revisão',
  ready_to_send: 'Pronto para 1-clique',
  sent: 'Enviado',
}

export const EQUIPMENT_LABELS: Record<Schemas['Equipment'], string> = {
  fcl_20dv: 'FCL 20DV',
  fcl_40hc: 'FCL 40HC',
  lcl: 'LCL',
  air: 'Aéreo',
}

export function formatMoney(money: Schemas['Money']): string {
  return new Intl.NumberFormat('pt-BR', { style: 'currency', currency: money.currency }).format(
    Number(money.amount),
  )
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
  const time = date.toLocaleTimeString('pt-BR', { hour: '2-digit', minute: '2-digit' })
  const daysAgo = Math.round((startOfDay(now) - startOfDay(date)) / (24 * 60 * MINUTE_MS))
  if (daysAgo === 0) return `hoje ${time}`
  if (daysAgo === 1) return `ontem ${time}`
  const day = date.toLocaleDateString('pt-BR', { day: '2-digit', month: '2-digit' })
  return `${day} ${time}`
}

export function formatRelative(iso: string, now: Date): string {
  const minutes = Math.max(0, Math.round((now.getTime() - new Date(iso).getTime()) / MINUTE_MS))
  if (minutes < 1) return 'agora'
  if (minutes < 60) return `há ${minutes} min`
  return `há ${Math.floor(minutes / 60)} h`
}
