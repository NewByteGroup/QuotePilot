import { BrowserRouter, Route, Routes } from 'react-router'

import { AppLayout } from '@/components/layout/app-layout'
import { ExchangePage } from '@/features/auth/exchange-page'
import { LoginPage } from '@/features/auth/login-page'
import { RequireAuth } from '@/features/auth/require-auth'
import { InboxPage } from '@/features/inbox/inbox-page'
import { ConnectorsPage } from '@/features/platform/connectors-page'
import { RepairsPage } from '@/features/platform/repairs-page'
import { ApprovalEmailPage } from '@/features/quote-audit/approval-email-page'
import { QuoteAuditPage } from '@/features/quote-audit/quote-audit-page'
import { IdentityTab } from '@/features/rates/identity-tab'
import { NetRatesTab } from '@/features/rates/net-rates-tab'
import { PricingRulesTab } from '@/features/rates/pricing-rules-tab'
import { RatesLayout } from '@/features/rates/rates-layout'

export function AppRoutes() {
  return (
    <Routes>
      <Route path="login" element={<LoginPage />} />
      <Route path="auth/exchange" element={<ExchangePage />} />
      <Route element={<RequireAuth />}>
        <Route element={<AppLayout />}>
          <Route index element={<InboxPage />} />
          <Route path="quotes/:quoteId" element={<QuoteAuditPage />} />
          <Route path="quotes/:quoteId/aprovacao" element={<ApprovalEmailPage />} />
          <Route path="tarifas" element={<RatesLayout />}>
            <Route index element={<NetRatesTab />} />
            <Route path="regras" element={<PricingRulesTab />} />
            <Route path="identidade" element={<IdentityTab />} />
          </Route>
          <Route path="conectores" element={<ConnectorsPage />} />
          <Route path="auto-reparo" element={<RepairsPage />} />
        </Route>
      </Route>
    </Routes>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <AppRoutes />
    </BrowserRouter>
  )
}
