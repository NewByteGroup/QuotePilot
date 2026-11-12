import { useTranslation } from 'react-i18next'

import { LANGUAGES } from '@/i18n'
import { cn } from '@/lib/utils'

export function LanguageSwitcher() {
  const { t, i18n } = useTranslation()
  return (
    <div
      role="radiogroup"
      aria-label={t('language.label')}
      className="bg-card flex rounded-full border p-0.5 shadow-xs"
    >
      {LANGUAGES.map((language) => {
        const active = i18n.language === language.code
        return (
          <button
            key={language.code}
            type="button"
            role="radio"
            aria-checked={active}
            aria-label={t(`language.names.${language.code}`)}
            onClick={() => void i18n.changeLanguage(language.code)}
            className={cn(
              'rounded-full px-2.5 py-1 text-xs font-semibold transition-colors',
              active
                ? 'bg-primary text-primary-foreground'
                : 'text-muted-foreground hover:text-foreground',
            )}
          >
            {language.label}
          </button>
        )
      })}
    </div>
  )
}
