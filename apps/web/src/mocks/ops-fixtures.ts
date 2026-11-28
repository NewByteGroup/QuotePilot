import type { CoreSchemas } from '@/api/client'

const CONNECTOR_BCB = '8d8f3f42-9c72-4f3e-b8df-1d32f841f101'
const CONNECTOR_SANTOS = '8d8f3f42-9c72-4f3e-b8df-1d32f841f102'
const CONNECTOR_MIRROR = '8d8f3f42-9c72-4f3e-b8df-1d32f841f103'

export const opsConnectorFixtures: CoreSchemas['OpsConnector'][] = [
  {
    id: CONNECTOR_BCB,
    slug: 'bcb_ptax',
    displayName: 'BCB PTAX',
    targetUrl: 'https://olinda.bcb.gov.br/olinda/servico/PTAX/versao/v1/odata',
    status: 'HEALTHY',
    confidenceScore: '0.998',
    lastScrapedAt: '2026-11-26T13:30:00Z',
    createdAt: '2026-11-10T12:00:00Z',
    updatedAt: '2026-11-26T13:30:00Z',
  },
  {
    id: CONNECTOR_SANTOS,
    slug: 'santos_brasil_tariffs',
    displayName: 'Santos Brasil · tarifas',
    targetUrl: 'https://www.santosbrasil.com.br/tarifas',
    status: 'DEGRADED',
    confidenceScore: '0.872',
    lastScrapedAt: '2026-11-25T18:10:00Z',
    createdAt: '2026-11-10T12:00:00Z',
    updatedAt: '2026-11-26T11:45:00Z',
  },
  {
    id: CONNECTOR_MIRROR,
    slug: 'demo_mirror_terminal',
    displayName: 'Terminal mirror · demo',
    targetUrl: 'https://mirror.quotepilot.example/terminal',
    status: 'HEALING',
    confidenceScore: '0.744',
    lastScrapedAt: '2026-11-26T10:20:00Z',
    createdAt: '2026-11-10T12:00:00Z',
    updatedAt: '2026-11-26T12:05:00Z',
  },
]

export const opsHealingFixtures: CoreSchemas['OpsHealingSession'][] = [
  {
    id: '9d1d70d2-64b5-4b34-a60a-bf7f4d3a7101',
    connectorId: CONNECTOR_MIRROR,
    status: 'PR_READY',
    failingSnapshotId: 'd926f6a2-9286-4f55-a6d0-b315f6bc5101',
    failingPayloadHash: '9f2d1a4f6d3d4b2d9d6d0f117f37f1a6f2d5f6f68d6325e493df28fd979981a2',
    failingPayloadStorageKey: 'platform/healing/demo_mirror_terminal/payload.html',
    devinSessionId: 'devin-demo-mirror-221',
    devinSessionUrl: 'https://app.devin.ai/sessions/demo-mirror-221',
    githubPrUrl: 'https://github.com/NewByteGroup/QuotePilot/pull/221',
    pytestPassed: true,
    createdAt: '2026-11-26T12:05:00Z',
    resolvedAt: null,
  },
  {
    id: '9d1d70d2-64b5-4b34-a60a-bf7f4d3a7102',
    connectorId: CONNECTOR_SANTOS,
    status: 'OPEN',
    failingSnapshotId: 'd926f6a2-9286-4f55-a6d0-b315f6bc5102',
    failingPayloadHash: 'a31d70d264b54b34a60abf7f4d3a7102a31d70d264b54b34a60abf7f4d3a7102',
    failingPayloadStorageKey: null,
    devinSessionId: 'devin-santos-222',
    devinSessionUrl: 'https://app.devin.ai/sessions/santos-222',
    githubPrUrl: null,
    pytestPassed: null,
    createdAt: '2026-11-26T11:50:00Z',
    resolvedAt: null,
  },
  {
    id: '9d1d70d2-64b5-4b34-a60a-bf7f4d3a7103',
    connectorId: CONNECTOR_BCB,
    status: 'MERGED',
    failingSnapshotId: null,
    failingPayloadHash: null,
    failingPayloadStorageKey: null,
    devinSessionId: 'devin-ptax-219',
    devinSessionUrl: 'https://app.devin.ai/sessions/ptax-219',
    githubPrUrl: 'https://github.com/NewByteGroup/QuotePilot/pull/219',
    pytestPassed: true,
    createdAt: '2026-11-24T15:15:00Z',
    resolvedAt: '2026-11-24T16:02:00Z',
  },
]

export const opsQuarantineFixtures: CoreSchemas['OpsQuarantineItem'][] = [
  {
    id: '0f32bb52-97a0-4b69-a002-b0a0a14e4101',
    resendMessageId: 're_msg_01JZQ6X7MULTI',
    reason: 'multi_match',
    recipients: ['quotes@importex.com.br', 'cotacoes@atlantica.com.br'],
    matchedOrgIds: ['3f9a5b6e-1c2d-4e5f-8a9b-0c1d2e3f4a5b', '40f3ef80-98c8-4d3d-a3a4-f23df8362102'],
    subject: 'Cotação importação FCL Santos → Paranaguá',
    fromAddr: 'comercial@shipperexample.com.br',
    receivedAt: '2026-11-26T12:40:00Z',
    quarantinedAt: '2026-11-26T12:40:04Z',
  },
  {
    id: '0f32bb52-97a0-4b69-a002-b0a0a14e4102',
    resendMessageId: 're_msg_01JZQ6X7NOMATCH',
    reason: 'no_match',
    recipients: ['quotes@unknown.example'],
    matchedOrgIds: [],
    subject: 'Atualização tarifária TCP dezembro',
    fromAddr: 'tarifas@tcp.example',
    receivedAt: '2026-11-26T09:12:00Z',
    quarantinedAt: '2026-11-26T09:12:03Z',
  },
]
