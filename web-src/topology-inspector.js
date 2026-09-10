const availabilityLabels = {
  unknown: 'Not reported',
  permission_denied: 'Phone permission not granted',
  unsupported: 'Not supported on this device',
}

export function availabilityLabel(value) {
  return availabilityLabels[value] || (value === 'available' ? 'Available' : 'Not reported')
}

export function networkCode(network = {}) {
  return network.mcc && network.mnc ? `MCC ${network.mcc} / MNC ${network.mnc}` : ''
}

export function registrationLabel(value) {
  return ({
    in_service: 'In service',
    out_of_service: 'Out of service',
    emergency_only: 'Emergency calls only',
    power_off: 'Radio powered off',
  })[value] || ''
}

export function signalLabel(signal = {}) {
  const parts = []
  if (Number.isInteger(signal.level) && signal.level >= 0 && signal.level <= 4) {
    parts.push(`${signal.level}/4 Android signal level`)
  }
  if (Number.isFinite(signal.dbm)) parts.push(`${signal.dbm} dBm`)
  return parts.join(' · ')
}

function networkRows(network = {}) {
  if (network.availability !== 'available') return [['Status', availabilityLabel(network.availability)]]
  return [
    ['Operator', network.operator],
    ['Network code', networkCode(network)],
    ['Measured', network.measured_at],
  ].filter(([, value]) => value !== '' && value !== undefined && value !== null)
}

function compactRows(rows) {
  return rows.filter(([, value]) => value !== '' && value !== undefined && value !== null)
}

export function buildCellularSections(cellular, cellularStatus = 'unknown') {
  if (cellularStatus === 'not_observed') {
    return [{
      title: 'Cellular radio',
      rows: [
        ['State', 'Not observed'],
        ['Reason', 'This source is used as a SIM/APDU reader; Viti does not use or inspect its cellular radio'],
      ],
    }]
  }
  if (!cellular) {
    return [{
      title: 'Cellular telemetry',
      rows: [['Status', 'Not reported by this Android agent']],
    }]
  }
  const serviceRows = cellular.service?.availability === 'available'
    ? compactRows([
        ['Registration', registrationLabel(cellular.service.registration)],
        ['Roaming', typeof cellular.service.roaming === 'boolean' ? (cellular.service.roaming ? 'Yes' : 'No') : ''],
        ['Service measured', cellular.service.measured_at],
      ])
    : [['Status', availabilityLabel(cellular.service?.availability)]]
  const access = cellular.data_access || {}
  const signal = cellular.signal || {}
  const radioRows = [
    ['Data technology', access.availability === 'available' ? access.technology : availabilityLabel(access.availability)],
    ['Signal', signal.availability === 'available' ? signalLabel(signal) : availabilityLabel(signal.availability)],
  ].filter(([, value]) => value)
  return [
    {
      title: 'Telemetry report',
      tone: cellular.stale || cellularStatus === 'stale' ? 'warning' : 'neutral',
      rows: compactRows([
        ['Freshness', cellular.stale || cellularStatus === 'stale' ? 'Stale — device is offline or the report has expired' : 'Current'],
        ['Subscription', cellular.subscription_role === 'default_data' ? 'Default-data subscription used for AKA' : 'No active data subscription'],
        ['Report measured', cellular.measured_at],
        ['Server received', cellular.received_at],
      ]),
    },
    { title: 'Home subscription', rows: networkRows(cellular.home_network) },
    { title: 'Serving cellular network', rows: networkRows(cellular.serving_network) },
    { title: 'Cellular service', rows: serviceRows },
    {
      title: 'Radio',
      rows: [
        ...radioRows,
        ['Technology measured', access.measured_at],
        ['Signal measured', signal.measured_at],
      ].filter(([, value]) => value),
    },
  ].filter(section => section.rows.length)
}
