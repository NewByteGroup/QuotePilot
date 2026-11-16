import { Check, ChevronRight, Lock } from 'lucide-react'
import { useId, useState, type FormEvent } from 'react'

import type { Schemas } from '@/api/client'
import { useUpdateUserPreferences } from '@/api/queries'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'

type UserLanguage = Schemas['UserLanguage']

const LANGUAGE_OPTIONS: { value: UserLanguage; label: string; hint: string }[] = [
  { value: 'pt', label: 'Português', hint: 'Brasil' },
  { value: 'en', label: 'English', hint: 'United States' },
  { value: 'es', label: 'Español', hint: 'España' },
]

function languageLabel(language: UserLanguage): string {
  return LANGUAGE_OPTIONS.find((option) => option.value === language)?.label ?? language
}

type SettingsDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  user: Schemas['SessionUser']
}

export function SettingsDialog({ open, onOpenChange, user }: SettingsDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        {open ? <SettingsForm user={user} onDone={() => onOpenChange(false)} /> : null}
      </DialogContent>
    </Dialog>
  )
}

function SettingsForm({ user, onDone }: { user: Schemas['SessionUser']; onDone: () => void }) {
  const nameId = useId()
  const emailId = useId()
  const [name, setName] = useState(user.name)
  const [language, setLanguage] = useState(user.language)
  const [languageOpen, setLanguageOpen] = useState(false)
  const update = useUpdateUserPreferences()

  const trimmedName = name.trim()
  const patch: Schemas['UpdateUserPreferencesRequest'] = {
    ...(trimmedName !== user.name ? { name: trimmedName } : {}),
    ...(language !== user.language ? { language } : {}),
  }
  const dirty = Object.keys(patch).length > 0
  const nameInvalid = trimmedName.length === 0

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    if (nameInvalid) return
    if (!dirty) return onDone()
    update.mutate(patch, { onSuccess: onDone })
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-5">
      <DialogHeader>
        <DialogTitle>Configurações</DialogTitle>
        <DialogDescription>Sua conta no QuotePilot.</DialogDescription>
      </DialogHeader>

      <div className="space-y-4">
        <div className="space-y-1.5">
          <label htmlFor={nameId} className="text-sm font-medium">
            Nome
          </label>
          <Input
            id={nameId}
            value={name}
            maxLength={120}
            autoComplete="name"
            aria-invalid={nameInvalid || undefined}
            onChange={(event) => setName(event.target.value)}
          />
          {nameInvalid ? (
            <p role="alert" className="text-xs text-red-700">
              Informe um nome.
            </p>
          ) : (
            <p className="text-muted-foreground text-xs">É o nome exibido na sidebar.</p>
          )}
        </div>

        <div className="space-y-1.5">
          <label htmlFor={emailId} className="text-sm font-medium">
            E-mail
          </label>
          <div className="relative">
            <Input id={emailId} value={user.email} readOnly className="bg-muted pr-9" />
            <Lock
              aria-hidden
              className="text-muted-foreground absolute top-1/2 right-3 size-4 -translate-y-1/2"
            />
          </div>
          <p className="text-muted-foreground text-xs">
            Sincronizado com a sua conta · não pode ser alterado aqui.
          </p>
        </div>

        <button
          type="button"
          onClick={() => setLanguageOpen(true)}
          aria-haspopup="dialog"
          className="hover:bg-muted flex w-full items-center justify-between rounded-md border px-3 py-2.5 text-left text-sm transition-colors"
        >
          <span className="font-medium">Idioma</span>
          <span className="text-muted-foreground flex items-center gap-1">
            {languageLabel(language)}
            <ChevronRight aria-hidden className="size-4" />
          </span>
        </button>
      </div>

      {update.isError ? (
        <p role="alert" className="text-sm text-red-700">
          Não foi possível salvar as configurações. Tente de novo.
        </p>
      ) : null}

      <DialogFooter>
        <Button type="button" variant="outline" onClick={onDone} disabled={update.isPending}>
          Cancelar
        </Button>
        <Button type="submit" disabled={update.isPending || nameInvalid}>
          {update.isPending ? 'Salvando…' : 'Salvar'}
        </Button>
      </DialogFooter>

      <LanguageDialog
        open={languageOpen}
        onOpenChange={setLanguageOpen}
        value={language}
        onSelect={(next) => {
          setLanguage(next)
          setLanguageOpen(false)
        }}
      />
    </form>
  )
}

type LanguageDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  value: UserLanguage
  onSelect: (language: UserLanguage) => void
}

function LanguageDialog({ open, onOpenChange, value, onSelect }: LanguageDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-xs">
        <DialogHeader>
          <DialogTitle>Idioma</DialogTitle>
          <DialogDescription>Idioma dos textos que o QuotePilot envia para você.</DialogDescription>
        </DialogHeader>
        <div role="radiogroup" aria-label="Idioma" className="grid gap-1">
          {LANGUAGE_OPTIONS.map((option) => {
            const selected = option.value === value
            return (
              <button
                key={option.value}
                type="button"
                role="radio"
                aria-checked={selected}
                onClick={() => onSelect(option.value)}
                className={cn(
                  'hover:bg-muted flex items-center justify-between rounded-md px-3 py-2 text-left text-sm transition-colors',
                  selected && 'bg-accent text-accent-foreground hover:bg-accent',
                )}
              >
                <span>
                  <span className="block font-medium">{option.label}</span>
                  <span className="text-muted-foreground block text-xs">{option.hint}</span>
                </span>
                {selected ? <Check aria-hidden className="size-4" /> : null}
              </button>
            )
          })}
        </div>
      </DialogContent>
    </Dialog>
  )
}
