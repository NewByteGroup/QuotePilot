import { Navigate, Outlet } from 'react-router'

import { useMe } from '@/api/auth'

// Gate for every signed-in route: no session (after one refresh attempt) goes to /login.
export function RequireAuth() {
  const me = useMe()
  if (me.isPending) {
    return (
      <p role="status" className="text-muted-foreground p-6 text-sm">
        Carregando sua sessão…
      </p>
    )
  }
  if (me.isError) {
    return (
      <p role="alert" className="p-6 text-sm text-red-700">
        Não foi possível verificar sua sessão. Recarregue a página.
      </p>
    )
  }
  if (me.data === null) return <Navigate to="/login" replace />
  return <Outlet />
}
