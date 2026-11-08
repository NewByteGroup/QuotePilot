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
    estimatedTotal: brl('23260.55'),
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

const PORT_NAMES: Record<string, string> = {
  BEANR: 'Antuérpia',
  BRGRU: 'Guarulhos',
  BRITJ: 'Itajaí',
  BRNVT: 'Navegantes',
  BRPNG: 'Paranaguá',
  BRRIG: 'Rio Grande',
  BRSSZ: 'Santos',
  BRSUA: 'Suape',
  BRVIX: 'Vitória',
  CNNSA: 'Nansha',
  CNQDG: 'Qingdao',
  CNSHA: 'Shanghai',
  CNSZX: 'Shenzhen',
  DEHAM: 'Hamburgo',
  DESTR: 'Stuttgart',
  INBOM: 'Mumbai',
  INNSA: 'Nhava Sheva',
  KRPUS: 'Busan',
  NLRTM: 'Roterdã',
  PTLIS: 'Lisboa',
  USMIA: 'Miami',
}

const EQUIPMENT_IN_EMAIL: Record<Schemas['Equipment'], string> = {
  fcl_20dv: "1×20'DV",
  fcl_40hc: "1×40'HC",
  lcl: 'LCL',
  air: 'carga aérea',
}

type Segment = Schemas['EmailSegment']
const plain = (text: string): Segment => ({ text, highlight: 'none' })
const extracted = (text: string): Segment => ({ text, highlight: 'extracted' })
const inferred = (text: string): Segment => ({ text, highlight: 'inferred' })

type CargoFacts = {
  incoterm: string
  ncm: string
  weightKg: number
  volumeM3: number
  goodsValueUsd: number
}

function cargoFactsFor(quote: Schemas['InboxQuote']): CargoFacts {
  if (quote.rfqNumber === 2481) {
    return {
      incoterm: 'EXW',
      ncm: '8479.89.99',
      weightKg: 12400,
      volumeM3: 58,
      goodsValueUsd: 45000,
    }
  }
  const n = quote.rfqNumber
  return {
    incoterm: n % 2 === 0 ? 'FOB' : 'EXW',
    ncm: '8481.80.99',
    weightKg: 8000 + (n % 7) * 1100,
    volumeM3: 30 + (n % 5) * 7,
    goodsValueUsd: 20000 + (n % 9) * 5000,
  }
}

const formatInt = (value: number) => value.toLocaleString('pt-BR')

function emailParagraphsFor(quote: Schemas['InboxQuote'], facts: CargoFacts): Segment[][] {
  const origin = PORT_NAMES[quote.route.origin] ?? quote.route.origin
  const destination = PORT_NAMES[quote.route.destination] ?? quote.route.destination
  const paragraphs: Segment[][] = [
    [plain('Bom dia,')],
    [
      plain('Preciso de cotação '),
      extracted(`${facts.incoterm} ${origin}`),
      plain(' para '),
      extracted(`${destination},`),
      plain(' '),
      extracted(`${EQUIPMENT_IN_EMAIL[quote.equipment]},`),
      plain(quote.rfqNumber === 2481 ? ' máquinas industriais, ' : ' '),
      extracted(`NCM ${facts.ncm}.`),
    ],
    [
      plain('Peso '),
      extracted(`${formatInt(facts.weightKg)} kg,`),
      plain(' cubagem '),
      extracted(`${facts.volumeM3} m³,`),
      plain(' valor FOB '),
      extracted(`USD ${formatInt(facts.goodsValueUsd)}.`),
    ],
    [plain('Embarque na '), inferred('semana do dia 18.')],
  ]
  if (quote.rfqNumber === 2481) {
    paragraphs.push([plain('Cliente novo — incluir seguro e observar validade.')])
    paragraphs.push([plain('Att., Renata (Importex Trading)')])
  } else {
    paragraphs.push([plain(`Att., Compras (${quote.customerName})`)])
  }
  return paragraphs
}

function extractionFor(
  quote: Schemas['InboxQuote'],
  facts: CargoFacts,
): Schemas['ExtractedField'][] {
  return [
    { label: 'Incoterm', value: facts.incoterm, inferred: false },
    { label: 'POL', value: quote.route.origin, inferred: false },
    { label: 'POD', value: quote.route.destination, inferred: false },
    { label: 'NCM', value: facts.ncm, inferred: false },
    { label: 'Peso', value: `${formatInt(facts.weightKg)} kg`, inferred: false },
    { label: 'Cubagem', value: `${facts.volumeM3} m³`, inferred: false },
    { label: 'Valor', value: `USD ${formatInt(facts.goodsValueUsd)}`, inferred: false },
    { label: 'Seguro', value: '0,35% assumido', inferred: true },
  ]
}

type ItemSeed = {
  label: string
  detail?: string
  cents: number
  currency?: string
  inferred?: boolean
}

// Price composition of RFQ #2481 in the Figma; other quotes scale it to their estimated total.
const REFERENCE_TOTAL_CENTS = 2326055

function priceSectionSeeds(quote: Schemas['InboxQuote']) {
  const destination = PORT_NAMES[quote.route.destination] ?? quote.route.destination
  const freightLabel =
    quote.equipment === 'air'
      ? 'Frete aéreo LATAM Cargo · tabela w44'
      : 'Frete marítimo Maersk · tabela w44'
  return [
    {
      title: 'Frete internacional',
      source: { label: 'tarifa privada · RLS', kind: 'private_rate' as const },
      note: null,
      items: [
        { label: freightLabel, detail: 'vigência 03–09/11', cents: 285000, currency: 'USD' },
        {
          label: 'Conversão · dólar armador 5,42',
          detail: 'PTAX 5,38 como referência',
          cents: 1544700,
        },
      ] satisfies ItemSeed[],
    },
    {
      title: `Taxas locais · ${destination}`,
      source: { label: 'público · snapshot', kind: 'public_snapshot' as const },
      note: `fonte: tabela pública ${destination} Brasil · snapshot 03/11 09:12`,
      items: [
        { label: 'THC destino', cents: 115000 },
        { label: 'Capatazia', cents: 48000 },
        { label: 'ISPS', cents: 9000 },
        { label: 'Doc fee', cents: 25000 },
        { label: 'Armazenagem (5 dias)', cents: 120000 },
      ] satisfies ItemSeed[],
    },
    {
      title: 'Tributos, adicionais e seguro',
      source: null,
      note: null,
      items: [
        { label: 'AFRMM (8% sobre o frete)', cents: 123576 },
        { label: 'IOF-câmbio (0,38%)', cents: 6194 },
        {
          label: 'Seguro (0,35% do FOB)',
          detail: 'inferido — não informado no pedido',
          cents: 85365,
          inferred: true,
        },
      ] satisfies ItemSeed[],
    },
  ]
}

function centsToMoney(cents: number, currency = 'BRL'): Schemas['Money'] {
  return { amount: (cents / 100).toFixed(2), currency }
}

function moneyToCents(money: Schemas['Money']): number {
  const [whole, fraction = ''] = money.amount.split('.')
  return Number(whole) * 100 + Number(fraction.padEnd(2, '0').slice(0, 2))
}

function pricingFor(quote: Schemas['InboxQuote']): Schemas['PriceComposition'] | null {
  if (!quote.estimatedTotal || quote.confidencePercent === null) return null
  const totalCents = moneyToCents(quote.estimatedTotal)
  const ratio = totalCents / REFERENCE_TOTAL_CENTS
  let costCents = 0
  const sections = priceSectionSeeds(quote).map((section) => ({
    title: section.title,
    source: section.source,
    note: section.note,
    items: section.items.map((item: ItemSeed) => {
      const cents = Math.round(item.cents * ratio)
      if (!item.currency) costCents += cents
      return {
        label: item.label,
        detail: item.detail ?? null,
        amount: centsToMoney(cents, item.currency),
        inferred: item.inferred ?? false,
      }
    }),
  }))
  return {
    sections,
    totalCost: centsToMoney(costCents),
    marginPercent: '12',
    margin: centsToMoney(totalCents - costCents),
    proposalTotal: centsToMoney(totalCents),
    confidencePercent: quote.confidencePercent,
  }
}

function emailDomainFor(customerName: string): string {
  const slug = customerName
    .normalize('NFD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(Boolean)[0]
  return `${slug}.com.br`
}

export function buildQuoteDetail(quote: Schemas['InboxQuote']): Schemas['QuoteDetail'] {
  const facts = cargoFactsFor(quote)
  return {
    quote,
    email: {
      from: `compras@${emailDomainFor(quote.customerName)}`,
      to: sessionFixture.organization.inboundAddress,
      receivedAt: quote.receivedAt,
      paragraphs: emailParagraphsFor(quote, facts),
      attachments: [
        { fileName: 'packing-list.pdf', sizeBytes: 182 * 1024 },
        { fileName: 'commercial-invoice.pdf', sizeBytes: 240 * 1024 },
      ],
    },
    extraction: extractionFor(quote, facts),
    pricing: pricingFor(quote),
  }
}
