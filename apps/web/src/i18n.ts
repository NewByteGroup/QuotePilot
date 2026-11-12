import i18n from 'i18next'
import { initReactI18next } from 'react-i18next'

import en from '@/languages/en.json'
import es from '@/languages/es.json'
import pt from '@/languages/pt.json'

export const LANGUAGES = [
  { code: 'pt', label: 'PT', locale: 'pt-BR' },
  { code: 'en', label: 'EN', locale: 'en-US' },
  { code: 'es', label: 'ES', locale: 'es-ES' },
] as const

export type LanguageCode = (typeof LANGUAGES)[number]['code']

const STORAGE_KEY = 'quotepilot.language'
const DEFAULT_LANGUAGE: LanguageCode = 'pt'

function isLanguageCode(value: unknown): value is LanguageCode {
  return LANGUAGES.some((language) => language.code === value)
}

function storedLanguage(): LanguageCode {
  const stored = globalThis.localStorage?.getItem(STORAGE_KEY)
  return isLanguageCode(stored) ? stored : DEFAULT_LANGUAGE
}

export function currentLocale(): string {
  return LANGUAGES.find((language) => language.code === i18n.language)?.locale ?? 'pt-BR'
}

void i18n.use(initReactI18next).init({
  resources: { pt: { translation: pt }, en: { translation: en }, es: { translation: es } },
  lng: storedLanguage(),
  fallbackLng: DEFAULT_LANGUAGE,
  interpolation: { escapeValue: false },
})

i18n.on('languageChanged', (language) => {
  if (isLanguageCode(language)) globalThis.localStorage?.setItem(STORAGE_KEY, language)
  document.documentElement.lang = currentLocale()
})
document.documentElement.lang = currentLocale()

export default i18n
