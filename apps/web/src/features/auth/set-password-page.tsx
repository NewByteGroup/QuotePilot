import { useId, useState, type FormEvent } from 'react'
import { Link, useSearchParams } from 'react-router'

import { InvalidCredentialsError, useSetPassword } from '@/api/auth'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

import { AuthShell } from './auth-shell'

const MIN_PASSWORD_LENGTH = 8 // PasswordSetRequest.password in core-api.yaml: 8..128

const loginLink = (label: string) => (
  <Link
    to="/login"
    className="text-primary block text-center text-sm underline-offset-4 hover:underline"
  >
    {label}
  </Link>
)

// Landing page of the reset link (`${WEB_URL}/auth/set-password?token=…`).
export function SetPasswordPage() {
  const [params] = useSearchParams()
  const token = params.get('token')
  const passwordId = useId()
  const confirmId = useId()
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [mismatch, setMismatch] = useState(false)
  const setNewPassword = useSetPassword()

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setMismatch(password !== confirm)
    if (password !== confirm || !token) return
    setNewPassword.mutate({ token, password })
  }

  if (setNewPassword.isSuccess) {
    return (
      <AuthShell
        title="Senha alterada"
        description={<span role="status">Agora é só entrar com a nova senha.</span>}
      >
        {loginLink('Ir para o login')}
      </AuthShell>
    )
  }

  if (!token || setNewPassword.error instanceof InvalidCredentialsError) {
    return (
      <AuthShell
        title="Link inválido ou expirado"
        description={
          <span role="alert">Peça um novo link de recuperação: ele vale uma única vez.</span>
        }
      >
        {loginLink('Voltar para o login')}
      </AuthShell>
    )
  }

  return (
    <AuthShell title="Criar nova senha" description="Use pelo menos 8 caracteres.">
      <form onSubmit={handleSubmit} className="space-y-4">
        <div className="space-y-1.5">
          <label htmlFor={passwordId} className="text-sm font-medium">
            Nova senha
          </label>
          <Input
            id={passwordId}
            type="password"
            required
            autoFocus
            minLength={MIN_PASSWORD_LENGTH}
            maxLength={128}
            autoComplete="new-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            className="h-10"
          />
        </div>
        <div className="space-y-1.5">
          <label htmlFor={confirmId} className="text-sm font-medium">
            Confirmar senha
          </label>
          <Input
            id={confirmId}
            type="password"
            required
            autoComplete="new-password"
            value={confirm}
            onChange={(event) => setConfirm(event.target.value)}
            className="h-10"
          />
        </div>
        {mismatch ? (
          <p role="alert" className="text-sm text-red-700">
            As senhas não conferem.
          </p>
        ) : null}
        {setNewPassword.isError ? (
          <p role="alert" className="text-sm text-red-700">
            Não foi possível salvar a senha. Tente de novo em instantes.
          </p>
        ) : null}
        <Button type="submit" className="h-10 w-full" disabled={setNewPassword.isPending}>
          {setNewPassword.isPending ? 'Salvando…' : 'Salvar nova senha'}
        </Button>
      </form>
    </AuthShell>
  )
}
