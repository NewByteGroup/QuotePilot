import type { Schemas } from '@/api/client'

const MINUTE_MS = 60_000

function minutesFrom(now: Date, minutes: number): string {
  return new Date(now.getTime() + minutes * MINUTE_MS).toISOString()
}

function brl(amount: string): Schemas['Money'] {
  return { amount, currency: 'BRL' }
}

export const sessionFixture: Schemas['Session'] = {
  user: {
    id: '7d1f0a52-6c39-4f7e-9a0e-0c5b8e2a1001',
    name: 'Marcelo Corner',
    role: 'admin',
    organizationCount: 2,
  },
  organization: {
    id: '3b4c5d6e-7f80-4912-a3b4-c5d6e7f80001',
    name: 'Atlântico Cargo',
    planName: 'Plano Operação',
    inboundAddress: 'atlantico@in.quotepilot.app',
  },
}

type QuoteSeed = Omit<Schemas['InboxQuote'], 'id' | 'slaDueAt' | 'receivedAt'> & {
  receivedMinutesAgo: number
  slaMinutesLeft: number | null
}

const QUOTE_SEEDS: QuoteSeed[] = [
  {
    rfqNumber: 2481,
    customerName: 'Importex Trading',
    subject: "Cotação EXW Shanghai → Santos · 1×40'HC",
    route: { origin: 'CNSHA', destination: 'BRSSZ' },
    equipment: 'fcl_40hc',
    status: 'ready_to_send',
    estimatedTotal: brl('23260.00'),
    confidencePercent: 98,
    slaAtRisk: false,
    receivedMinutesAgo: 5,
    slaMinutesLeft: 72,
  },
  {
    rfqNumber: 2480,
    customerName: 'Verde Agro Insumos',
    subject: 'Ureia granulada 25 t · Paranaguá',
    route: { origin: 'CNQDG', destination: 'BRPNG' },
    equipment: 'fcl_20dv',
    status: 'awaiting_review',
    estimatedTotal: brl('18940.00'),
    confidencePercent: 87,
    slaAtRisk: false,
    receivedMinutesAgo: 49,
    slaMinutesLeft: 125,
  },
  {
    rfqNumber: 2479,
    customerName: 'MedSupply Brasil',
    subject: 'Equip. hospitalar FOB Miami',
    route: { origin: 'USMIA', destination: 'BRSSZ' },
    equipment: 'air',
    status: 'processing',
    estimatedTotal: null,
    confidencePercent: null,
    slaAtRisk: false,
    receivedMinutesAgo: 94,
    slaMinutesLeft: 220,
  },
  {
    rfqNumber: 2478,
    customerName: 'TechParts Ltda',
    subject: 'Autopeças EXW Stuttgart',
    route: { origin: 'DESTR', destination: 'BRITJ' },
    equipment: 'fcl_20dv',
    status: 'ready_to_send',
    estimatedTotal: brl('11402.00'),
    confidencePercent: 100,
    slaAtRisk: false,
    receivedMinutesAgo: 60 * 17,
    slaMinutesLeft: 250,
  },
  {
    rfqNumber: 2477,
    customerName: 'Polímeros Sul',
    subject: 'Resina PEAD · 2 contêineres',
    route: { origin: 'KRPUS', destination: 'BRPNG' },
    equipment: 'fcl_40hc',
    status: 'awaiting_review',
    estimatedTotal: brl('41880.00'),
    confidencePercent: 91,
    slaAtRisk: true,
    receivedMinutesAgo: 60 * 19,
    slaMinutesLeft: 48,
  },
  {
    rfqNumber: 2476,
    customerName: 'Casa & Décor Import',
    subject: 'Móveis desmontados · 68 m³',
    route: { origin: 'CNNSA', destination: 'BRSSZ' },
    equipment: 'lcl',
    status: 'sent',
    estimatedTotal: brl('9760.00'),
    confidencePercent: 96,
    slaAtRisk: false,
    receivedMinutesAgo: 60 * 21,
    slaMinutesLeft: null,
  },
  {
    rfqNumber: 2475,
    customerName: 'Café Export BR',
    subject: 'Café em grãos CIF Hamburgo',
    route: { origin: 'BRSSZ', destination: 'DEHAM' },
    equipment: 'fcl_20dv',
    status: 'sent',
    estimatedTotal: brl('15320.00'),
    confidencePercent: 99,
    slaAtRisk: false,
    receivedMinutesAgo: 60 * 24 * 7,
    slaMinutesLeft: null,
  },
  {
    rfqNumber: 2474,
    customerName: 'Nordeste Têxtil',
    subject: 'Fios de algodão · Suape',
    route: { origin: 'INNSA', destination: 'BRSUA' },
    equipment: 'fcl_40hc',
    status: 'processing',
    estimatedTotal: null,
    confidencePercent: null,
    slaAtRisk: false,
    receivedMinutesAgo: 20,
    slaMinutesLeft: 280,
  },
  {
    rfqNumber: 2473,
    customerName: 'Vinícola Serra',
    subject: 'Garrafas vazias · Lisboa → Rio Grande',
    route: { origin: 'PTLIS', destination: 'BRRIG' },
    equipment: 'fcl_20dv',
    status: 'awaiting_review',
    estimatedTotal: brl('12730.00'),
    confidencePercent: 89,
    slaAtRisk: false,
    receivedMinutesAgo: 140,
    slaMinutesLeft: 160,
  },
  {
    rfqNumber: 2472,
    customerName: 'Mineração Vale Verde',
    subject: 'Peças de reposição · Antuérpia',
    route: { origin: 'BEANR', destination: 'BRVIX' },
    equipment: 'fcl_40hc',
    status: 'ready_to_send',
    estimatedTotal: brl('27140.00'),
    confidencePercent: 97,
    slaAtRisk: false,
    receivedMinutesAgo: 170,
    slaMinutesLeft: 190,
  },
  {
    rfqNumber: 2471,
    customerName: 'Pharma Norte',
    subject: 'Insumos farmacêuticos · Mumbai',
    route: { origin: 'INBOM', destination: 'BRGRU' },
    equipment: 'air',
    status: 'sent',
    estimatedTotal: brl('8450.00'),
    confidencePercent: 95,
    slaAtRisk: false,
    receivedMinutesAgo: 60 * 26,
    slaMinutesLeft: null,
  },
  {
    rfqNumber: 2470,
    customerName: 'AutoLog Brasil',
    subject: 'Pneus · Busan → Itajaí',
    route: { origin: 'KRPUS', destination: 'BRITJ' },
    equipment: 'fcl_40hc',
    status: 'sent',
    estimatedTotal: brl('31500.00'),
    confidencePercent: 98,
    slaAtRisk: false,
    receivedMinutesAgo: 60 * 28,
    slaMinutesLeft: null,
  },
  {
    rfqNumber: 2469,
    customerName: 'Doce Lar Alimentos',
    subject: 'Açúcar VHP · Santos → Roterdã',
    route: { origin: 'BRSSZ', destination: 'NLRTM' },
    equipment: 'fcl_20dv',
    status: 'sent',
    estimatedTotal: brl('14210.00'),
    confidencePercent: 100,
    slaAtRisk: false,
    receivedMinutesAgo: 60 * 24 * 8,
    slaMinutesLeft: null,
  },
  {
    rfqNumber: 2468,
    customerName: 'Eletro Sul',
    subject: 'Componentes eletrônicos · Shenzhen',
    route: { origin: 'CNSZX', destination: 'BRNVT' },
    equipment: 'lcl',
    status: 'sent',
    estimatedTotal: brl('6980.00'),
    confidencePercent: 97,
    slaAtRisk: false,
    receivedMinutesAgo: 60 * 24 * 9,
    slaMinutesLeft: null,
  },
]

function toInboxQuote(seed: QuoteSeed, index: number, now: Date): Schemas['InboxQuote'] {
  const { receivedMinutesAgo, slaMinutesLeft, ...quote } = seed
  return {
    ...quote,
    id: `9f0e8d7c-6b5a-4c3d-8e2f-${String(index + 1).padStart(12, '0')}`,
    receivedAt: minutesFrom(now, -receivedMinutesAgo),
    slaDueAt: slaMinutesLeft === null ? null : minutesFrom(now, slaMinutesLeft),
  }
}

export function buildInboxQuotes(now: Date): Schemas['InboxQuote'][] {
  return QUOTE_SEEDS.map((seed, index) => toInboxQuote(seed, index, now))
}

export function buildInboxSummary(quotes: Schemas['InboxQuote'][]): Schemas['InboxSummary'] {
  const atRisk = quotes.filter((quote) => quote.slaAtRisk && quote.slaDueAt !== null)
  const mostUrgent = atRisk.sort((a, b) => (a.slaDueAt ?? '').localeCompare(b.slaDueAt ?? ''))[0]
  return {
    receivedToday: quotes.length,
    receivedYesterday: quotes.length - 3,
    readyToSend: quotes.filter((quote) => quote.status === 'ready_to_send').length,
    autoApproveConfidencePercent: 95,
    avgResponseMinutes: 11,
    responseTargetMinutes: 15,
    slaAtRiskCount: atRisk.length,
    mostUrgentSlaRisk: mostUrgent
      ? { customerName: mostUrgent.customerName, slaDueAt: mostUrgent.slaDueAt ?? '' }
      : null,
  }
}

export function countByStatus(quotes: Schemas['InboxQuote'][]): Schemas['InboxStatusCounts'] {
  const count = (status: Schemas['QuoteStatus']) => quotes.filter((q) => q.status === status).length
  return {
    all: quotes.length,
    processing: count('processing'),
    awaiting_review: count('awaiting_review'),
    ready_to_send: count('ready_to_send'),
    sent: count('sent'),
  }
}
