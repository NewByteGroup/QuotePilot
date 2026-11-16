import { useEffect, useRef } from 'react'
import { Link, Navigate, useSearchParams } from 'react-router'

import { useExchangeLoginToken } from '@/api/auth'

import { AuthShell } from './auth-shell'

// Landing page of the magic link (`${WEB_URL}/auth/exchange?token=…`): the token is
// single-use, so it is posted exactly once even under StrictMode's double effect.
export function ExchangePage() {
  const [params] = useSearchParams()
  const token = params.get('token')
  const exchange = useExchangeLoginToken()
  const sent = useRef(false)
  const { mutate } = exchange

  useEffect(() => {
    if (!token || sent.current) return
    sent.current = true
    mutate(token)
  }, [token, mutate])

  if (exchange.isSuccess) return <Navigate to="/" replace />

  if (!token || exchange.isError) {
    return (
      <AuthShell title="Link inválido ou expirado">
        <p role="alert" className="text-muted-foreground text-sm">
          Este link de acesso não é mais válido. Links valem por poucos minutos e só podem ser
          usados uma vez.
        </p>
        <Link to="/login" className="text-primary text-sm underline">
          Pedir um novo link
        </Link>
      </AuthShell>
    )
  }

  return (
    <AuthShell title="Entrando…">
      <p role="status" className="text-muted-foreground text-sm">
        Validando o seu link de acesso.
      </p>
    </AuthShell>
  )
}
