import { useEffect, useRef } from 'react'
import { Loader2 } from 'lucide-react'
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
      <AuthShell
        title="Link inválido ou expirado"
        description={
          <span role="alert">
            Este link de acesso não é mais válido. Links valem por poucos minutos e só podem ser
            usados uma vez.
          </span>
        }
      >
        <Link
          to="/login"
          className="bg-primary text-primary-foreground hover:bg-primary/90 flex h-11 w-full items-center justify-center rounded-md text-sm font-medium transition-colors"
        >
          Voltar para o login
        </Link>
      </AuthShell>
    )
  }

  return (
    <AuthShell
      title="Entrando…"
      description={<span role="status">Validando o seu link de acesso.</span>}
    >
      <Loader2 className="text-primary mx-auto size-8 animate-spin" />
    </AuthShell>
  )
}
