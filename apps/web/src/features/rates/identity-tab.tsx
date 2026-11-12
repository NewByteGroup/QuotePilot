import { useEffect, useId, useRef, useState, type ChangeEvent, type FormEvent } from 'react'
import type { Schemas } from '@/api/client'
import {
  useCompanyIdentity,
  useRemoveLetterheadLogo,
  useUpdateCompanyIdentity,
  useUploadLetterheadLogo,
} from '@/api/queries'
import { Panel, QueryFallback } from '@/components/panel'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { formatCnpj, initials } from '@/lib/format'
import { cn } from '@/lib/utils'

const CNPJ_PATTERN = /^\d{2}\.\d{3}\.\d{3}\/\d{4}-\d{2}$/
const LOGO_TYPES = ['image/png', 'image/jpeg', 'image/svg+xml']
const LOGO_MAX_BYTES = 2 * 1024 * 1024

type Draft = {
  legalName: string
  taxId: string
  address: string
  primaryColor: string
  signatureTitle: string
  signatureDetails: string
}

type LogoChange =
  { kind: 'none' } | { kind: 'upload'; file: File; previewUrl: string } | { kind: 'remove' }

function draftFrom(identity: Schemas['CompanyIdentity']): Draft {
  return {
    legalName: identity.legalName,
    taxId: identity.taxId,
    address: identity.address,
    primaryColor: identity.primaryColor,
    signatureTitle: identity.signature.title,
    signatureDetails: identity.signature.details,
  }
}

function changedFields(
  identity: Schemas['CompanyIdentity'],
  draft: Draft,
): Schemas['UpdateCompanyIdentityRequest'] {
  const patch: Schemas['UpdateCompanyIdentityRequest'] = {}
  if (draft.legalName.trim() !== identity.legalName) patch.legalName = draft.legalName.trim()
  if (draft.taxId !== identity.taxId) patch.taxId = draft.taxId
  if (draft.address.trim() !== identity.address) patch.address = draft.address.trim()
  if (draft.primaryColor !== identity.primaryColor) patch.primaryColor = draft.primaryColor
  const signature = { title: draft.signatureTitle.trim(), details: draft.signatureDetails.trim() }
  if (
    signature.title !== identity.signature.title ||
    signature.details !== identity.signature.details
  ) {
    patch.signature = signature
  }
  return patch
}

function validationError(draft: Draft): string | null {
  if (!draft.legalName.trim() || !draft.address.trim()) return 'Preencha razão social e endereço.'
  if (!CNPJ_PATTERN.test(draft.taxId)) return 'CNPJ inválido: use 14 dígitos.'
  if (!draft.signatureTitle.trim()) return 'Preencha a linha principal da assinatura.'
  return null
}

function readAsDataUrl(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(reader.error)
    reader.readAsDataURL(file)
  })
}

function Field({
  label,
  value,
  onChange,
  placeholder,
  inputMode,
}: {
  label: string
  value: string
  onChange: (value: string) => void
  placeholder?: string
  inputMode?: 'numeric'
}) {
  const id = useId()
  return (
    <div className="space-y-1">
      <label htmlFor={id} className="text-muted-foreground text-xs">
        {label}
      </label>
      <Input
        id={id}
        value={value}
        placeholder={placeholder}
        inputMode={inputMode}
        onChange={(event) => onChange(event.target.value)}
      />
    </div>
  )
}

function IdentityForm({ identity }: { identity: Schemas['CompanyIdentity'] }) {
  const update = useUpdateCompanyIdentity()
  const upload = useUploadLetterheadLogo()
  const remove = useRemoveLetterheadLogo()
  const inputRef = useRef<HTMLInputElement>(null)
  const fileInputId = useId()

  const [draft, setDraft] = useState(() => draftFrom(identity))
  const [logo, setLogo] = useState<LogoChange>({ kind: 'none' })
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)

  const patch = changedFields(identity, draft)
  const dirty = Object.keys(patch).length > 0 || logo.kind !== 'none'
  const saving = update.isPending || upload.isPending || remove.isPending
  const logoUrl =
    logo.kind === 'upload'
      ? logo.previewUrl
      : logo.kind === 'remove'
        ? null
        : identity.letterheadLogoUrl

  useEffect(() => {
    if (!dirty) return
    const warn = (event: BeforeUnloadEvent) => event.preventDefault()
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])

  function change(next: Partial<Draft>) {
    setDraft((current) => ({ ...current, ...next }))
    setSaved(false)
  }

  async function handleFile(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0]
    event.target.value = ''
    if (!file) return
    if (!LOGO_TYPES.includes(file.type)) return setError('Formato inválido: use PNG, JPEG ou SVG.')
    if (file.size > LOGO_MAX_BYTES) return setError('Arquivo grande demais: envie até 2 MB.')
    setError(null)
    setSaved(false)
    setLogo({ kind: 'upload', file, previewUrl: await readAsDataUrl(file) })
  }

  function discard() {
    setDraft(draftFrom(identity))
    setLogo({ kind: 'none' })
    setError(null)
  }

  async function handleSubmit(event: FormEvent) {
    event.preventDefault()
    const invalid = validationError(draft)
    if (invalid) return setError(invalid)
    setError(null)
    try {
      if (Object.keys(patch).length > 0) await update.mutateAsync(patch)
      if (logo.kind === 'upload') await upload.mutateAsync(logo.file)
      if (logo.kind === 'remove') await remove.mutateAsync()
      setLogo({ kind: 'none' })
      setSaved(true)
    } catch {
      setError('Não foi possível salvar as alterações. Tente de novo.')
    }
  }

  return (
    <form onSubmit={handleSubmit} noValidate className="space-y-4">
      <Panel title="Dados da empresa">
        <div className="flex items-start gap-4 p-4">
          <span
            className="flex size-12 shrink-0 items-center justify-center rounded-md text-sm font-semibold text-white"
            style={{ backgroundColor: draft.primaryColor }}
          >
            {initials(draft.legalName)}
          </span>
          <div className="grid flex-1 gap-3 sm:grid-cols-2">
            <Field
              label="Razão social"
              value={draft.legalName}
              onChange={(legalName) => change({ legalName })}
            />
            <Field
              label="CNPJ"
              value={draft.taxId}
              inputMode="numeric"
              placeholder="00.000.000/0000-00"
              onChange={(taxId) => change({ taxId: formatCnpj(taxId) })}
            />
            <div className="sm:col-span-2">
              <Field
                label="Endereço"
                value={draft.address}
                onChange={(address) => change({ address })}
              />
            </div>
          </div>
        </div>
      </Panel>

      <Panel title="Cor primária">
        <div className="space-y-3 p-4 text-sm">
          <p className="text-muted-foreground">
            Selecione a cor de destaque usada em CTAs e acentos
          </p>
          <div role="radiogroup" aria-label="Cor primária" className="flex gap-3">
            {identity.primaryColorOptions.map((color) => {
              const selected = color === draft.primaryColor
              return (
                <button
                  key={color}
                  type="button"
                  role="radio"
                  aria-checked={selected}
                  aria-label={`Cor ${color}`}
                  onClick={() => change({ primaryColor: color })}
                  className={cn(
                    'size-7 rounded-full transition-shadow',
                    selected
                      ? 'ring-foreground ring-2 ring-offset-2'
                      : 'hover:ring-border hover:ring-2 hover:ring-offset-2',
                  )}
                  style={{ backgroundColor: color }}
                />
              )
            })}
          </div>
          <p className="text-muted-foreground text-xs">
            PDF timbrado e e-mails usam esta cor como destaque.
          </p>
        </div>
      </Panel>

      <Panel title="Assinatura">
        <div className="grid gap-3 p-4 sm:grid-cols-2">
          <Field
            label="Linha principal"
            value={draft.signatureTitle}
            onChange={(signatureTitle) => change({ signatureTitle })}
          />
          <Field
            label="Detalhes"
            value={draft.signatureDetails}
            onChange={(signatureDetails) => change({ signatureDetails })}
          />
        </div>
      </Panel>

      <Panel
        title="Timbrado PDF"
        action={
          <div className="flex gap-2">
            {logoUrl ? (
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => {
                  setSaved(false)
                  setLogo(identity.letterheadLogoUrl ? { kind: 'remove' } : { kind: 'none' })
                }}
              >
                Remover
              </Button>
            ) : null}
            <Button type="button" size="sm" onClick={() => inputRef.current?.click()}>
              {logoUrl ? 'Trocar logo' : 'Enviar logo'}
            </Button>
          </div>
        }
      >
        <div className="space-y-2 p-4">
          <label htmlFor={fileInputId} className="sr-only">
            Arquivo do logo
          </label>
          <input
            ref={inputRef}
            id={fileInputId}
            type="file"
            accept={LOGO_TYPES.join(',')}
            className="sr-only"
            onChange={handleFile}
          />
          {logoUrl ? (
            <img
              src={logoUrl}
              alt="Logo da empresa"
              className="h-20 max-w-40 rounded-md border object-contain p-2"
            />
          ) : (
            <div className="bg-muted/50 text-muted-foreground flex h-20 w-40 items-center justify-center rounded-md border text-xs">
              Logo da empresa
            </div>
          )}
          <p className="text-muted-foreground text-xs">PNG, JPEG ou SVG, até 2 MB.</p>
        </div>
      </Panel>

      <div className="bg-card sticky bottom-4 flex flex-wrap items-center justify-between gap-3 rounded-lg border px-4 py-3 shadow-sm">
        <div className="text-sm">
          {error ? (
            <p role="alert" className="text-red-700">
              {error}
            </p>
          ) : (
            <p role="status" className={cn(dirty ? 'text-amber-700' : 'text-muted-foreground')}>
              {saving
                ? 'Salvando…'
                : dirty
                  ? 'Você tem alterações não salvas.'
                  : saved
                    ? 'Alterações salvas.'
                    : 'Nenhuma alteração pendente.'}
            </p>
          )}
        </div>
        <div className="flex gap-2">
          <Button type="button" variant="outline" disabled={!dirty || saving} onClick={discard}>
            Descartar
          </Button>
          <Button type="submit" disabled={!dirty || saving}>
            Salvar alterações
          </Button>
        </div>
      </div>
    </form>
  )
}

export function IdentityTab() {
  const identity = useCompanyIdentity()
  if (!identity.data) {
    return <QueryFallback isError={identity.isError} what="a identidade" />
  }
  return <IdentityForm identity={identity.data} />
}
