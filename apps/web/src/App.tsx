import { BrowserRouter, Route, Routes } from 'react-router'

import { AppLayout } from '@/components/layout/app-layout'
import { InboxPage } from '@/features/inbox/inbox-page'
import { QuoteAuditPage } from '@/features/quote-audit/quote-audit-page'

export function AppRoutes() {
  return (
    <Routes>
      <Route element={<AppLayout />}>
        <Route index element={<InboxPage />} />
        <Route path="quotes/:quoteId" element={<QuoteAuditPage />} />
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
