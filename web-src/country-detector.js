import { getCountriesForTimezone } from 'countries-and-timezones'

const ISO_COUNTRY = /^[A-Z]{2}$/

export const isCanonicalE164 = value => /^\+[1-9][0-9]{7,14}$/.test(String(value || '').trim())

// *43 is the explicit local echo feature. *44 is only emitted by the separate
// Join action and cannot be entered through the public-number normalization path.
export const isInternalUserDialTarget = value => String(value || '').trim() === '*43'

export function normalizeCountryCode(value) {
  const country = String(value || '').trim().toUpperCase()
  return ISO_COUNTRY.test(country) ? country : ''
}

export function countryFromLocale(locale, IntlObject = Intl) {
  try {
    return normalizeCountryCode(new IntlObject.Locale(locale).region)
  } catch {
    return ''
  }
}

export function countryFromTimezone(timezone) {
  const countries = getCountriesForTimezone(String(timezone || '').trim()) || []
  return countries.length === 1 ? normalizeCountryCode(countries[0]?.id) : ''
}

export function resolveCountrySignals({ ipCountry = '', timezoneCountry = '', localeCountry = '' } = {}) {
  const ip = normalizeCountryCode(ipCountry)
  const timezone = normalizeCountryCode(timezoneCountry)
  const locale = normalizeCountryCode(localeCountry)
  if (!ip) return { country: '', source: '', reason: 'ip-unavailable' }
  if (timezone) {
    if (timezone !== ip) return { country: '', source: '', reason: 'ip-timezone-conflict' }
    return { country: ip, source: 'ip+timezone', reason: '' }
  }
  if (locale === ip) return { country: ip, source: 'ip+locale', reason: '' }
  return { country: '', source: '', reason: locale ? 'ip-locale-conflict' : 'insufficient-evidence' }
}

async function responseTextWithinLimit(response, maximumBytes = 1024) {
  const declared = Number(response.headers?.get?.('content-length') || 0)
  if (declared > maximumBytes) throw new Error('country response is too large')
  const text = await response.text()
  if (new TextEncoder().encode(text).length > maximumBytes) throw new Error('country response is too large')
  return text
}

export async function lookupIPCountry({ fetchImpl = fetch, timeoutMs = 3500 } = {}) {
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const response = await fetchImpl('https://api.country.is/', {
      method: 'GET',
      headers: { Accept: 'application/json' },
      credentials: 'omit',
      referrerPolicy: 'no-referrer',
      cache: 'no-store',
      mode: 'cors',
      redirect: 'error',
      signal: controller.signal,
    })
    if (!response.ok) return ''
    const body = JSON.parse(await responseTextWithinLimit(response))
    return normalizeCountryCode(body?.country)
  } catch {
    return ''
  } finally {
    clearTimeout(timeout)
  }
}

export async function detectSessionCountry({
  fetchImpl = fetch,
  navigatorObject = navigator,
  IntlObject = Intl,
  timeoutMs = 3500,
} = {}) {
  const localeCountry = countryFromLocale(navigatorObject?.language || '', IntlObject)
  let timezoneCountry = ''
  try {
    timezoneCountry = countryFromTimezone(IntlObject.DateTimeFormat().resolvedOptions().timeZone)
  } catch {}
  const ipCountry = await lookupIPCountry({ fetchImpl, timeoutMs })
  return resolveCountrySignals({ ipCountry, timezoneCountry, localeCountry })
}
