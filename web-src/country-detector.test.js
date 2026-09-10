import test from 'node:test'
import assert from 'node:assert/strict'
import {
  countryFromLocale,
  countryFromTimezone,
  detectSessionCountry,
  isCanonicalE164,
  isInternalUserDialTarget,
  lookupIPCountry,
  resolveCountrySignals,
} from './country-detector.js'

test('only canonical international numbers and the private echo route bypass normalization', () => {
  assert.equal(isCanonicalE164('+34612345678'), true)
  assert.equal(isCanonicalE164('+34 612 345 678'), false)
  assert.equal(isInternalUserDialTarget('*43'), true)
  assert.equal(isInternalUserDialTarget('*44'), false)
  assert.equal(isInternalUserDialTarget('112'), false)
})

test('unique timezone and IP agree even when UI locale differs', () => {
  assert.deepEqual(resolveCountrySignals({ ipCountry: 'es', timezoneCountry: 'ES', localeCountry: 'US' }), {
    country: 'ES', source: 'ip+timezone', reason: '',
  })
})

test('VPN or exit-node conflict fails closed', () => {
  assert.deepEqual(resolveCountrySignals({ ipCountry: 'US', timezoneCountry: 'ES', localeCountry: 'ES' }), {
    country: '', source: '', reason: 'ip-timezone-conflict',
  })
})

test('locale corroborates IP only when timezone is not country-specific', () => {
  assert.equal(countryFromTimezone('UTC'), '')
  assert.equal(countryFromLocale('es-ES'), 'ES')
  assert.deepEqual(resolveCountrySignals({ ipCountry: 'ES', localeCountry: 'ES' }), {
    country: 'ES', source: 'ip+locale', reason: '',
  })
})

test('IP absence and malformed responses never invent a country', async () => {
  assert.deepEqual(resolveCountrySignals({ timezoneCountry: 'ES', localeCountry: 'ES' }), {
    country: '', source: '', reason: 'ip-unavailable',
  })
  const country = await lookupIPCountry({ fetchImpl: async () => ({ ok: true, headers: new Headers(), text: async () => '{bad' }) })
  assert.equal(country, '')
})

test('session detection uses a privacy-scoped direct request and returns no raw IP', async () => {
  let request
  const result = await detectSessionCountry({
    navigatorObject: { language: 'es-ES' },
    IntlObject: {
      Locale: Intl.Locale,
      DateTimeFormat: () => ({ resolvedOptions: () => ({ timeZone: 'Europe/Madrid' }) }),
    },
    fetchImpl: async (url, options) => {
      request = { url, options }
      return { ok: true, headers: new Headers(), text: async () => '{"ip":"203.0.113.7","country":"ES"}' }
    },
  })
  assert.deepEqual(result, { country: 'ES', source: 'ip+timezone', reason: '' })
  assert.equal(request.url, 'https://api.country.is/')
  assert.equal(request.options.credentials, 'omit')
  assert.equal(request.options.referrerPolicy, 'no-referrer')
  assert.equal(request.options.cache, 'no-store')
  assert.equal(request.options.redirect, 'error')
  assert.equal(JSON.stringify(result).includes('203.0.113.7'), false)
})

test('oversized country responses fail closed', async () => {
  const country = await lookupIPCountry({
    fetchImpl: async () => ({
      ok: true,
      headers: new Headers({ 'content-length': '4096' }),
      text: async () => '{"country":"ES"}',
    }),
  })
  assert.equal(country, '')
})

test('fresh sessions detect travel again instead of reusing the previous country', async () => {
  const sessionInSpain = await detectSessionCountry({
    navigatorObject: { language: 'es-ES' },
    IntlObject: {
      Locale: Intl.Locale,
      DateTimeFormat: () => ({ resolvedOptions: () => ({ timeZone: 'Europe/Madrid' }) }),
    },
    fetchImpl: async () => ({ ok: true, headers: new Headers(), text: async () => '{"country":"ES"}' }),
  })
  const sessionInBritain = await detectSessionCountry({
    navigatorObject: { language: 'en-GB' },
    IntlObject: {
      Locale: Intl.Locale,
      DateTimeFormat: () => ({ resolvedOptions: () => ({ timeZone: 'Europe/London' }) }),
    },
    fetchImpl: async () => ({ ok: true, headers: new Headers(), text: async () => '{"country":"GB"}' }),
  })

  assert.equal(sessionInSpain.country, 'ES')
  assert.equal(sessionInBritain.country, 'GB')
})
