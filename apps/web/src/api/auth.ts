import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { coreClient, type CoreSchemas } from './client'

export const ME_KEY = ['me']

async function fetchMe() {
  return coreClient.GET('/me')
}

// null means "not signed in". An expired access cookie is rotated once through
// /auth/refresh before giving up (core-api.yaml: 15-minute qp_access, 30-day qp_refresh).
async function loadMe(): Promise<CoreSchemas['MeResponse'] | null> {
  let result = await fetchMe()
  if (result.response.status === 401) {
    const refreshed = await coreClient.POST('/auth/refresh')
    if (!refreshed.response.ok) return null
    result = await fetchMe()
  }
  if (result.response.status === 401) return null
  if (!result.data)
    throw new Error(`GET /me failed: ${result.error?.error ?? result.response.status}`)
  return result.data
}

export function useMe() {
  return useQuery({ queryKey: ME_KEY, queryFn: loadMe, retry: false })
}

export class InvalidCredentialsError extends Error {}
export class RateLimitedError extends Error {}

function failure(operation: string, status: number, code?: string): Error {
  if (status === 401) return new InvalidCredentialsError(`${operation}: ${code ?? status}`)
  if (status === 429) return new RateLimitedError(`${operation}: ${code ?? status}`)
  return new Error(`${operation} failed: ${code ?? status}`)
}

// The password path of POST /auth/login answers like POST /auth/session: cookies plus
// the profile and memberships (ADR-005 amendment 2026-11-16).
export function usePasswordLogin() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (credentials: { email: string; password: string }) => {
      const { data, error, response } = await coreClient.POST('/auth/login', {
        body: credentials,
      })
      if (!data || !('profile' in data)) {
        throw failure('POST /auth/login', response.status, error?.error)
      }
      return data
    },
    onSuccess: ({ profile, memberships }) => {
      queryClient.setQueryData<CoreSchemas['MeResponse']>(ME_KEY, { ...profile, memberships })
    },
  })
}

export function useRequestPasswordReset() {
  return useMutation({
    mutationFn: async (email: string) => {
      const { data, error, response } = await coreClient.POST('/auth/password-reset', {
        body: { email },
      })
      if (!data) throw failure('POST /auth/password-reset', response.status, error?.error)
      return data
    },
  })
}

// Consumes the reset link token; no session starts, the user logs in with the new password.
export function useSetPassword() {
  return useMutation({
    mutationFn: async (body: { token: string; password: string }) => {
      const { data, error, response } = await coreClient.POST('/auth/password', { body })
      if (!data) throw failure('POST /auth/password', response.status, error?.error)
      return data
    },
  })
}

export function useExchangeLoginToken() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (token: string) => {
      const { data, error, response } = await coreClient.POST('/auth/session', { body: { token } })
      if (!data) throw new Error(`POST /auth/session failed: ${error?.error ?? response.status}`)
      return data
    },
    onSuccess: ({ profile, memberships }) => {
      const me: CoreSchemas['MeResponse'] = { ...profile, memberships }
      queryClient.setQueryData(ME_KEY, me)
    },
  })
}

export function useLogout() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async () => {
      await coreClient.POST('/auth/logout')
    },
    // Signed out either way: the server answers 204 with or without a live session.
    onSettled: () => {
      queryClient.setQueryData(ME_KEY, null)
      queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== ME_KEY[0] })
    },
  })
}
