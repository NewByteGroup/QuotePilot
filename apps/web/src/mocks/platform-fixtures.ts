import type { Schemas } from '@/api/client'

const DAY_MS = 86_400_000
const CNPJ_PATTERN = /^\d{2}\.\d{3}\.\d{3}\/\d{4}-\d{2}$/

export const LETTERHEAD_LOGO_TYPES = ['image/png', 'image/jpeg', 'image/svg+xml']
export const LETTERHEAD_LOGO_MAX_BYTES = 2 * 1024 * 1024

export function applyIdentityUpdate(
  identity: Schemas['CompanyIdentity'],
  patch: Schemas['UpdateCompanyIdentityRequest'],
): { identity: Schemas['CompanyIdentity'] } | { error: string } {
  if (patch.legalName !== undefined && !patch.legalName.trim()) {
    return { error: 'legalName is required' }
  }
  if (patch.address !== undefined && !patch.address.trim()) {
    return { error: 'address is required' }
  }
  if (patch.taxId !== undefined && !CNPJ_PATTERN.test(patch.taxId)) {
    return { error: 'taxId must be a formatted CNPJ' }
  }
  if (
    patch.primaryColor !== undefined &&
    !identity.primaryColorOptions.includes(patch.primaryColor)
  ) {
    return { error: 'primaryColor must be one of primaryColorOptions' }
  }
  if (patch.signature !== undefined && !patch.signature.title.trim()) {
    return { error: 'signature.title is required' }
  }
  return { identity: { ...identity, ...patch } }
}

export async function fileToDataUrl(file: Blob): Promise<string> {
  const bytes = new Uint8Array(await file.arrayBuffer())
  let binary = ''
  bytes.forEach((byte) => {
    binary += String.fromCharCode(byte)
  })
  return `data:${file.type};base64,${btoa(binary)}`
}

function at(now: Date, hours: number, minutes: number, dayOffset = 0): string {
  const date = new Date(now)
  date.setDate(date.getDate() + dayOffset)
  date.setHours(hours, minutes, 0, 0)
  return date.toISOString()
}

function shiftDays(now: Date, days: number): Date {
  return new Date(now.getTime() + days * DAY_MS)
}

function isoDate(date: Date): string {
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${date.getFullYear()}-${month}-${day}`
}

function isoWeek(date: Date): number {
  const utc = new Date(Date.UTC(date.getFullYear(), date.getMonth(), date.getDate()))
  const weekday = utc.getUTCDay() || 7
  utc.setUTCDate(utc.getUTCDate() + 4 - weekday)
  const yearStart = Date.UTC(utc.getUTCFullYear(), 0, 1)
  return Math.ceil(((utc.getTime() - yearStart) / DAY_MS + 1) / 7)
}

type NetRateSeed = {
  carrier: string
  lane: string
  file: string
  extension: string
  fromDay: number
  toDay: number
  status: Schemas['NetRateTableStatus']
  processingPercent?: number
}

const NET_RATE_SEEDS: NetRateSeed[] = [
  {
    carrier: 'Maersk',
    lane: 'Ásia → Santos',
    file: 'maersk',
    extension: 'pdf',
    fromDay: -1,
    toDay: 5,
    status: 'active',
  },
  {
    carrier: 'MSC',
    lane: 'Ásia → Paranaguá',
    file: 'msc',
    extension: 'csv',
    fromDay: -1,
    toDay: 5,
    status: 'active',
  },
  {
    carrier: 'Hapag-Lloyd',
    lane: 'Europa → Itajaí',
    file: 'hl',
    extension: 'pdf',
    fromDay: -5,
    toDay: 2,
    status: 'expiring',
  },
  {
    carrier: 'CMA CGM',
    lane: 'Ásia → Santos',
    file: 'cma',
    extension: 'xlsx',
    fromDay: -12,
    toDay: -6,
    status: 'expired',
  },
  {
    carrier: 'ONE',
    lane: 'Ásia → Santos',
    file: 'one',
    extension: 'pdf',
    fromDay: 0,
    toDay: 6,
    status: 'processing',
    processingPercent: 64,
  },
]

export function buildNetRateTablePage(now: Date): Schemas['NetRateTablePage'] {
  return {
    items: NET_RATE_SEEDS.map((seed, index) => {
      const validFrom = shiftDays(now, seed.fromDay)
      const week = `w${isoWeek(validFrom)}`
      return {
        id: `5d1e7c42-8b0a-4c1e-9f00-00000000000${index + 1}`,
        carrier: seed.carrier,
        lane: seed.lane,
        weekLabel: week,
        validFrom: isoDate(validFrom),
        validTo: isoDate(shiftDays(now, seed.toDay)),
        uploadedAt: seed.status === 'processing' ? at(now, 9, 2) : at(now, 8, 30, seed.fromDay - 1),
        fileName: `${seed.file}-${week}.${seed.extension}`,
        status: seed.status,
        processingPercent: seed.processingPercent ?? null,
      }
    }),
    staleQuoteCount: 3,
  }
}

const DEFAULT_MARGIN = '12.0'

function routeMargin(origin: string, destination: string, applied: string): Schemas['RouteMargin'] {
  return {
    route: { origin, destination },
    defaultMarginPercent: DEFAULT_MARGIN,
    appliedMarginPercent: applied,
    customized: applied !== DEFAULT_MARGIN,
  }
}

export function buildPricingRules(): Schemas['PricingRules'] {
  return {
    targetMarginPercent: DEFAULT_MARGIN,
    fxSpreadPercent: '0.50',
    dispatchFee: { amount: '350.00', currency: 'BRL' },
    marginFloorPercent: '8.0',
    routeMargins: [
      routeMargin('CNSHA', 'BRSSZ', '12.0'),
      routeMargin('KRPUS', 'BRPNG', '10.8'),
      routeMargin('DESTR', 'BRITJ', '15.2'),
      routeMargin('USMIA', 'BRSSZ', '14.0'),
      routeMargin('CNQDG', 'BRPNG', '11.5'),
    ],
    automations: [
      {
        key: 'auto_approve_high_confidence',
        label: 'Aprovar automaticamente se confiança ≥ 95%',
        enabled: true,
      },
      {
        key: 'recalculate_on_fx_change',
        label: 'Recalcular se o câmbio variar > 2%',
        enabled: true,
      },
      {
        key: 'block_negative_margin',
        label: 'Bloquear proposta com margem negativa',
        enabled: true,
      },
      { key: 'alert_sla_under_30_min', label: 'Alertar se o SLA restante < 30 min', enabled: true },
      {
        key: 'apply_global_floor',
        label: 'Aplicar o piso global a todos os clientes',
        enabled: true,
      },
    ],
  }
}

export function addRouteMargins(
  rules: Schemas['PricingRules'],
  routes: Schemas['NewRouteMargin'][],
): Schemas['PricingRules'] {
  const added = routes.map(({ route, marginPercent }) => ({
    route,
    defaultMarginPercent: rules.targetMarginPercent,
    appliedMarginPercent: marginPercent ?? rules.marginFloorPercent,
    customized: true,
  }))
  const isAdded = (route: Schemas['Route']) =>
    added.some(
      (item) => item.route.origin === route.origin && item.route.destination === route.destination,
    )
  return {
    ...rules,
    routeMargins: [...rules.routeMargins.filter((item) => !isAdded(item.route)), ...added],
  }
}

const PRIMARY_COLOR_OPTIONS = ['#0f172a', '#2563eb', '#0d9488', '#171717']

// Each organization has its own identity; the second one gets generic data derived from its name.
export function buildCompanyIdentity(
  organization?: Schemas['SessionOrganization'],
): Schemas['CompanyIdentity'] {
  if (organization && organization.name !== 'Atlântico Cargo') {
    return {
      legalName: `${organization.name} Ltda`,
      taxId: '98.765.432/0001-10',
      address: 'Av. Beira-Mar, 200 · Itajaí/SC · Brasil',
      primaryColor: '#0d9488',
      primaryColorOptions: PRIMARY_COLOR_OPTIONS,
      signature: {
        title: `${organization.name} Ltda — Logística Internacional`,
        details: 'Itajaí · SC · Brasil · (47) 0000-0000',
      },
      letterheadLogoUrl: null,
    }
  }
  return {
    legalName: 'Atlântico Cargo Ltda',
    taxId: '12.345.678/0001-90',
    address: 'Av. Paulista, 1000 · São Paulo/SP · Brasil',
    primaryColor: '#0f172a',
    primaryColorOptions: PRIMARY_COLOR_OPTIONS,
    signature: {
      title: 'Atlântico Cargo Ltda — Comércio Exterior',
      details: 'São Paulo · SP · Brasil · (11) 0000-0000',
    },
    letterheadLogoUrl: null,
  }
}

function tcpRepairTimeline(now: Date): Schemas['RepairTimeline'] {
  return {
    connectorName: 'TCP Paranaguá',
    events: [
      {
        at: at(now, 9, 41),
        kind: 'anomaly',
        message: 'Anomalia detectada: score 93% (< 95%) na tabela landside',
        tag: 'anomalia',
        sessionUrl: null,
      },
      {
        at: at(now, 9, 41),
        kind: 'session_opened',
        message: 'Sessão Devin #221 aberta · snapshot antigo vs novo + teste que falhou',
        tag: 'sessão #221',
        sessionUrl: 'https://app.devin.ai/sessions/example-221',
      },
      {
        at: at(now, 9, 47),
        kind: 'tests_passed',
        message: 'Parser corrigido · pytest 12/12 verde',
        tag: 'testes',
        sessionUrl: null,
      },
      {
        at: at(now, 9, 52),
        kind: 'pull_request_merged',
        message: 'PR #84 merged após revisão humana (Pedro)',
        tag: 'PR #84',
        sessionUrl: null,
      },
      {
        at: at(now, 9, 53),
        kind: 'reprocessed',
        message: 'Conector restaurado · 3 cotações reprocessadas',
        tag: 'reprocessado',
        sessionUrl: null,
      },
    ],
  }
}

type ConnectorSeed = [
  name: string,
  kind: Schemas['ConnectorKind'],
  healthPercent: number,
  latencyMs: number,
  collectedAt: [hours: number, minutes: number],
  detail?: string,
  repairing?: boolean,
]

const CONNECTOR_SEEDS: ConnectorSeed[] = [
  ['Câmbio diário · armadores', 'fx', 100, 800, [9, 12], '6 armadores'],
  ['Santos Brasil · armazenagem/THC', 'public_tariff', 96, 1400, [9, 0]],
  ['BTP · tabela V3.2026', 'public_tariff', 99, 1100, [9, 0]],
  ['TCP Paranaguá · landside', 'public_tariff', 93, 2200, [8, 40], undefined, true],
  ['Portonave · capatazia', 'public_tariff', 100, 900, [9, 5]],
  ['PTAX · Banco Central', 'fx_reference', 100, 400, [9, 10]],
]

export function buildConnectorOverview(now: Date): Schemas['ConnectorOverview'] {
  return {
    stats: {
      activeSources: 18,
      sourceKindsSummary: 'câmbio, terminais, sobretaxas, PTAX',
      averageHealthPercent: '98.2',
      degradedSourceNames: ['TCP'],
      repairs7d: 5,
      repairMeanMinutes: 12,
      snapshots24h: 1204,
      collectionFailures24h: 0,
    },
    connectors: CONNECTOR_SEEDS.map(
      ([name, kind, healthPercent, latencyMs, [hours, minutes], detail, repairing], index) => ({
        id: `8a3f0b6d-2c4e-4f1a-b000-00000000000${index + 1}`,
        name,
        kind,
        healthPercent,
        latencyMs,
        collectedAt: at(now, hours, minutes),
        detail: detail ?? null,
        repairing: repairing ?? false,
      }),
    ),
    liveRepair: tcpRepairTimeline(now),
  }
}

export function buildRepairOverview(now: Date): Schemas['RepairOverview'] {
  const session = (
    sessionNumber: number,
    connectorName: string,
    anomaly: string,
    result: Schemas['RepairSession']['result'],
    durationMinutes: number,
    finishedAt: string,
  ): Schemas['RepairSession'] => ({
    sessionNumber,
    connectorName,
    anomaly,
    result,
    durationMinutes,
    finishedAt,
  })

  return {
    stats: { repairs7d: 5, successRatePercent: 100, escalated7d: 0, mttrMinutes: 12, queued: 3 },
    current: tcpRepairTimeline(now),
    queue: [
      { connectorName: 'TCP Paranaguá', state: 'in_progress', scheduledAt: null },
      { connectorName: 'BTP · tabela V3.2026', state: 'waiting_slot', scheduledAt: null },
      { connectorName: 'Portonave · capatazia', state: 'scheduled', scheduledAt: at(now, 9, 0, 1) },
    ],
    history: [
      session(221, 'TCP Paranaguá', 'score 93% (< 95%)', 'resolved', 12, at(now, 9, 53)),
      session(220, 'BTP · tabela V3.2026', 'timeout na coleta', 'resolved', 6, at(now, 8, 14)),
      session(
        219,
        'Santos Brasil · armazenagem/THC',
        'mudança de layout na tabela',
        'resolved',
        18,
        at(now, 16, 20, -1),
      ),
      session(
        218,
        'Portonave · capatazia',
        'captcha no portal',
        'escalated',
        45,
        at(now, 11, 5, -8),
      ),
      session(
        217,
        'Câmbio diário · armadores',
        'divergência de 2 fontes',
        'resolved',
        9,
        at(now, 10, 30, -9),
      ),
    ],
  }
}
