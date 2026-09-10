import test from 'node:test'
import assert from 'node:assert/strict'
import { buildCellularSections, networkCode, registrationLabel, signalLabel } from './topology-inspector.js'

test('OpenStick explains that cellular radio is not observed without inventing metrics', () => {
  const sections = buildCellularSections(null, 'not_observed')
  assert.equal(sections.length, 1)
  assert.deepEqual(sections[0].rows[0], ['State', 'Not observed'])
  assert.doesNotMatch(JSON.stringify(sections), /LTE|Registered|dBm/)
})

test('remote Android observations keep home, serving, service and radio concepts separate', () => {
  const sections = buildCellularSections({
    subscription_role: 'default_data',
    home_network: { availability: 'available', operator: 'O2', mcc: '214', mnc: '07' },
    serving_network: { availability: 'available', operator: 'Movistar', mcc: '214', mnc: '07' },
    service: { availability: 'available', registration: 'in_service', roaming: false },
    data_access: { availability: 'available', technology: 'LTE' },
    signal: { availability: 'available', level: 3, dbm: -91 },
    stale: false,
  }, 'observed')
  assert.deepEqual(sections.map(section => section.title), [
    'Telemetry report', 'Home subscription', 'Serving cellular network', 'Cellular service', 'Radio',
  ])
  assert.match(JSON.stringify(sections), /O2/)
  assert.match(JSON.stringify(sections), /Movistar/)
  assert.match(JSON.stringify(sections), /3\/4 Android signal level/)
})

test('unavailable values explain absence instead of becoming successful state', () => {
  const sections = buildCellularSections({
    subscription_role: 'default_data',
    home_network: { availability: 'unknown' },
    serving_network: { availability: 'unknown' },
    service: { availability: 'permission_denied' },
    data_access: { availability: 'permission_denied' },
    signal: { availability: 'unknown' },
    stale: false,
  }, 'observed')
  const text = JSON.stringify(sections)
  assert.match(text, /Phone permission not granted/)
  assert.match(text, /Not reported/)
  assert.doesNotMatch(text, /Operational|In service/)
})

test('an expired field remains distinct from a current telemetry report', () => {
  const sections = buildCellularSections({
    subscription_role: 'default_data',
    home_network: { availability: 'unknown' },
    serving_network: { availability: 'unknown' },
    service: { availability: 'unknown' },
    data_access: { availability: 'available', technology: 'LTE', measured_at: '2026-01-01T00:04:59Z' },
    signal: { availability: 'stale', measured_at: '2026-01-01T00:00:00Z' },
    stale: false,
  }, 'observed')
  const text = JSON.stringify(sections)
  assert.match(text, /Report freshness.*Current/)
  assert.match(text, /Signal status.*Measurement expired/)
  assert.match(text, /Signal measured.*2026-01-01T00:00:00Z/)
  assert.doesNotMatch(text, /dBm|Android signal level/)
})

test('formatters expose raw OS evidence without claiming inferred coverage', () => {
  assert.equal(networkCode({ mcc: '214', mnc: '07' }), 'MCC 214 / MNC 07')
  assert.equal(registrationLabel('in_service'), 'In service')
  assert.equal(signalLabel({ level: 4, dbm: -72 }), '4/4 Android signal level · -72 dBm')
})
