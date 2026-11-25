import { useEffect, useRef, useState } from 'react'
import { Loader2 } from 'lucide-react'
import { Link, Navigate, useSearchParams } from 'react-router'

import { useExchangeLoginToken, useMe } from '@/api/auth'
import { Button } from '@/components/ui/button'

import { AuthShell } from './auth-shell'

// Landing page of the magic link (`${WEB_URL}/auth/exchange?token=…`): the token is
// single-use, so it is posted exactly once even under StrictMode's double effect.
export function ExchangePage() {
  const [params] = useSearchParams()
  const token = params.get('token')
  const exchange = useExchangeLoginToken()
  const me = useMe()
  const [confirmed, setConfirmed] = useState(false)
  const sent = useRef(false)
  const { mutate } = exchange

  // A live session must confirm before the exchange swaps identities —
  // otherwise one forwarded link silently replaces it.
  const needsConfirm = Boolean(me.data) && !confirmed

  useEffect(() => {
    if (!token || sent.current || me.isPending || needsConfirm) return
    sent.current = true
    mutate(token)
  }, [token, mutate, me.isPending, me.data, needsConfirm])

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

  if (needsConfirm) {
    return (
      <AuthShell
        title="Trocar de conta?"
        description={
          <span>
            Você está conectado como <strong>{me.data?.email}</strong>. Este link inicia outra
            sessão e encerra a atual.
          </span>
        }
      >
        <div className="space-y-3">
          <Button className="h-10 w-full" onClick={() => setConfirmed(true)}>
            Entrar com o link
          </Button>
          <Link
            to="/"
            className="text-muted-foreground block text-center text-sm underline-offset-4 hover:underline"
          >
            Cancelar
          </Link>
        </div>
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
