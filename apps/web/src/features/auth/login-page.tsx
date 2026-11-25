import { Eye, EyeOff } from 'lucide-react'
import { useId, useState, type FormEvent } from 'react'
import { Link, Navigate } from 'react-router'

import { InvalidCredentialsError, RateLimitedError, useMe, usePasswordLogin } from '@/api/auth'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { CORE_MOCK_ENABLED } from '@/lib/mock-flag'
import { MOCK_PASSWORD } from '@/mocks/demo-credentials'

import { AuthShell } from './auth-shell'

export function LoginPage() {
  const emailId = useId()
  const passwordId = useId()
  const me = useMe()
  const login = usePasswordLogin()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [showPassword, setShowPassword] = useState(false)

  if (me.data) return <Navigate to="/" replace />

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    login.mutate({ email: email.trim(), password })
  }

  return (
    <AuthShell title="Entrar" description="Acesse a sua conta para continuar.">
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
        <div className="space-y-1.5">
          <div className="flex items-center justify-between">
            <label htmlFor={passwordId} className="text-sm font-medium">
              Senha
            </label>
            <Link
              to="/recuperar-senha"
              className="text-primary text-sm underline-offset-4 hover:underline"
            >
              Esqueci minha senha
            </Link>
          </div>
          <div className="relative">
            <Input
              id={passwordId}
              type={showPassword ? 'text' : 'password'}
              required
              autoComplete="current-password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              className="h-10 pr-10"
            />
            <button
              type="button"
              aria-label={showPassword ? 'Ocultar senha' : 'Mostrar senha'}
              onClick={() => setShowPassword((shown) => !shown)}
              className="text-muted-foreground hover:text-foreground absolute top-1/2 right-3 -translate-y-1/2"
            >
              {showPassword ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
            </button>
          </div>
        </div>
        {login.isError ? (
          <p role="alert" className="text-sm text-red-700">
            {loginErrorMessage(login.error)}
          </p>
        ) : null}
        <Button type="submit" className="h-10 w-full" disabled={login.isPending}>
          {login.isPending ? 'Entrando…' : 'Entrar'}
        </Button>
        {CORE_MOCK_ENABLED ? (
          <p className="text-muted-foreground text-center text-xs">
            Demonstração: qualquer e-mail com a senha{' '}
            <code className="text-foreground">{MOCK_PASSWORD}</code>
          </p>
        ) : null}
      </form>
    </AuthShell>
  )
}

function loginErrorMessage(error: Error): string {
  if (error instanceof InvalidCredentialsError) return 'E-mail ou senha incorretos.'
  if (error instanceof RateLimitedError) return 'Muitas tentativas. Aguarde alguns minutos.'
  return 'Não foi possível entrar agora. Tente de novo em instantes.'
}
