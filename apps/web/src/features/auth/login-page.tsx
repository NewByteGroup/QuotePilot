import { useId, useState, type FormEvent } from 'react'
import { Link, Navigate } from 'react-router'

import { useMe, useRequestLoginLink } from '@/api/auth'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { MOCK_ENABLED } from '@/lib/mock-flag'
import { MOCK_LOGIN_TOKEN } from '@/mocks/login-token'

import { AuthShell } from './auth-shell'

export function LoginPage() {
  const emailId = useId()
  const me = useMe()
  const [email, setEmail] = useState('')
  const requestLink = useRequestLoginLink()

  if (me.data) return <Navigate to="/" replace />

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    requestLink.mutate(email.trim())
  }

  // The answer is the same whether or not the account exists (core-api never reveals it).
  if (requestLink.isSuccess) {
    return (
      <AuthShell title="Confira seu e-mail">
        <p role="status" className="text-muted-foreground text-sm">
          Se houver uma conta para <strong className="text-foreground">{email.trim()}</strong>,
          enviamos um link de acesso. Ele vale por poucos minutos e só pode ser usado uma vez.
        </p>
        {MOCK_ENABLED ? (
          <Link
            to={`/auth/exchange?token=${MOCK_LOGIN_TOKEN}`}
            className="text-primary text-sm underline"
          >
            Modo demonstração: abrir o link de acesso
          </Link>
        ) : null}
        <Button type="button" variant="outline" className="w-full" onClick={requestLink.reset}>
          Usar outro e-mail
        </Button>
      </AuthShell>
    )
  }

  return (
    <AuthShell title="Entrar">
      <form onSubmit={handleSubmit} className="space-y-4">
        <div className="space-y-1.5">
          <label htmlFor={emailId} className="text-sm font-medium">
            E-mail
          </label>
          <Input
            id={emailId}
            type="email"
            required
            autoFocus
            autoComplete="email"
            placeholder="voce@empresa.com.br"
            value={email}
            onChange={(event) => setEmail(event.target.value)}
          />
          <p className="text-muted-foreground text-xs">
            Enviamos um link de acesso para o seu e-mail. Sem senha.
          </p>
        </div>
        {requestLink.isError ? (
          <p role="alert" className="text-sm text-red-700">
            Não foi possível enviar o link. Aguarde um pouco e tente de novo.
          </p>
        ) : null}
        <Button type="submit" className="w-full" disabled={requestLink.isPending}>
          {requestLink.isPending ? 'Enviando…' : 'Enviar link de acesso'}
        </Button>
      </form>
    </AuthShell>
  )
}
