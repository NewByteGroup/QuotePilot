import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useTranslation } from 'react-i18next'
import { describe, expect, it } from 'vitest'

import { LanguageSwitcher } from '@/components/language-switcher'
import i18n from '@/i18n'
import en from '@/languages/en.json'
import es from '@/languages/es.json'
import pt from '@/languages/pt.json'

function keys(value: object, prefix = ''): string[] {
  return Object.entries(value)
    .flatMap(([key, child]) =>
      typeof child === 'object' && child !== null
        ? keys(child as object, `${prefix}${key}.`)
        : [`${prefix}${key}`],
    )
    .sort()
}

function Title() {
  const { t } = useTranslation()
  return <h1>{t('inbox.title')}</h1>
}

describe('i18n', () => {
  it('has the same keys in every language', () => {
    expect(keys(en)).toEqual(keys(pt))
    expect(keys(es)).toEqual(keys(pt))
  })

  it('switches the whole UI language and remembers the choice', async () => {
    render(
      <>
        <LanguageSwitcher />
        <Title />
      </>,
    )
    expect(screen.getByRole('heading')).toHaveTextContent('Inbox de cotações')

    await userEvent.click(screen.getByRole('radio', { name: 'Inglês' }))
    expect(screen.getByRole('heading')).toHaveTextContent('Quote inbox')
    expect(localStorage.getItem('quotepilot.language')).toBe('en')
    expect(document.documentElement.lang).toBe('en-US')

    await userEvent.click(screen.getByRole('radio', { name: 'Spanish' }))
    expect(screen.getByRole('heading')).toHaveTextContent('Bandeja de cotizaciones')
    expect(i18n.language).toBe('es')
  })
})
