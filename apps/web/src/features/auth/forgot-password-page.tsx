import { useId, useState, type FormEvent } from 'react'
import { Link } from 'react-router'

import { useRequestPasswordReset } from '@/api/auth'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

import { AuthShell } from './auth-shell'

const backToLogin = (
  <Link
    to="/login"
    className="text-primary block text-center text-sm underline-offset-4 hover:underline"
  >
    Voltar para o login
  </Link>
)

export function ForgotPasswordPage() {
  const emailId = useId()
  const [email, setEmail] = useState('')
  const reset = useRequestPasswordReset()

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    reset.mutate(email.trim())
  }

  // Same answer whether or not the account exists, so the page never leaks who has one.
  if (reset.isSuccess) {
    return (
      <AuthShell
        title="Confira seu e-mail"
        description={
          <span role="status">
            Se houver uma conta para <strong className="text-foreground">{email.trim()}</strong>,
            enviamos um link para criar uma nova senha.
          </span>
        }
      >
        {backToLogin}
      </AuthShell>
    )
  }

  return (
    <AuthShell
      title="Recuperar senha"
      description="Informe o e-mail da sua conta e enviaremos um link para criar uma nova senha."
    >
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
            className="h-10"
          />
        </div>
        {reset.isError ? (
          <p role="alert" className="text-sm text-red-700">
            Não foi possível enviar o link. Tente de novo em instantes.
          </p>
        ) : null}
        <Button type="submit" className="h-10 w-full" disabled={reset.isPending}>
          {reset.isPending ? 'Enviando…' : 'Enviar link de recuperação'}
        </Button>
        {backToLogin}
      </form>
    </AuthShell>
  )
}
