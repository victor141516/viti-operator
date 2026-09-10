const finite = (value) => typeof value === 'number' && Number.isFinite(value)
const audio = (item) => (item.kind || item.mediaType) === 'audio' && item.active !== false
const percent = (lost, total) => total > 0 ? Math.round((lost / total) * 10_000) / 100 : null
const milliseconds = (seconds) => finite(seconds) && seconds >= 0 ? Math.round(seconds * 100_000) / 100 : null

function reportsMap(stats) {
  const result = new Map()
  if (typeof stats?.forEach === 'function') stats.forEach((item) => result.set(item.id, item))
  else for (const item of stats || []) result.set(item.id, item)
  return result
}

function intervalMetric(items, previous, receivedField) {
  let received = 0
  let lost = 0
  let earliest = Infinity
  let latest = -Infinity
  let continuing = false
  for (const item of items) {
    const before = previous.get(item.id)
    if (!before || before.lost < 0 || !finite(item[receivedField]) || !finite(item.packetsLost) || item.packetsLost < 0 || !finite(item.timestamp)) continue
    const receivedDelta = item[receivedField] - before.received
    const lostDelta = item.packetsLost - before.lost
    const elapsed = item.timestamp - before.timestamp
    if (elapsed <= 0 || receivedDelta < 0 || lostDelta < 0) continue
    continuing = true
    received += receivedDelta
    lost += lostDelta
    earliest = Math.min(earliest, before.timestamp)
    latest = Math.max(latest, item.timestamp)
  }
  const denominator = received + lost
  return {
    value: continuing && denominator > 0 ? percent(lost, denominator) : null,
    windowMs: continuing && latest >= earliest ? latest - earliest : null,
  }
}

function nextCounters(items, receivedField) {
  const result = new Map()
  for (const item of items) {
    if (finite(item[receivedField]) && finite(item.packetsLost) && item.packetsLost >= 0 && finite(item.timestamp)) {
      result.set(item.id, { received: item[receivedField], lost: item.packetsLost, timestamp: item.timestamp })
    }
  }
  return result
}

function resolveRoute(reports, rtp) {
  const exactPairIds = new Set()
  for (const item of rtp) {
    const transport = reports.get(item.transportId)
    if (transport?.selectedCandidatePairId) exactPairIds.add(transport.selectedCandidatePairId)
  }
  let pair = null
  if (exactPairIds.size === 1) pair = reports.get([...exactPairIds][0])
  else if (exactPairIds.size > 1) return null
  else {
    const legacy = [...reports.values()].filter((item) => item.type === 'candidate-pair' && (
      item.selected === true || (item.nominated === true && item.state === 'succeeded')
    ))
    if (legacy.length !== 1) return null
    pair = legacy[0]
  }
  if (!pair) return null
  const local = reports.get(pair.localCandidateId)
  const remote = reports.get(pair.remoteCandidateId)
  if (!local || !remote) return null
  const localType = String(local.candidateType || '')
  const remoteType = String(remote.candidateType || '')
  const relayed = localType === 'relay' || remoteType === 'relay'
  const protocol = String((localType === 'relay' && local.relayProtocol) || local.protocol || remote.protocol || '').toLowerCase()
  return {
    pairId: pair.id || '',
    route: {
      mode: relayed ? 'turn-relay' : 'direct',
      protocol: protocol || 'unknown',
      localCandidateType: localType || 'unknown',
      remoteCandidateType: remoteType || 'unknown',
    },
  }
}

function currentJitter(items, previous) {
  let weighted = 0
  let weight = 0
  for (const item of items) {
    const before = previous.get(item.id)
    if (!before || !finite(item.jitter) || !finite(item.packetsReceived)) continue
    const delta = item.packetsReceived - before.received
    if (delta <= 0) continue
    weighted += item.jitter * delta
    weight += delta
  }
  return weight > 0 ? milliseconds(weighted / weight) : null
}

function mediaRoundTrip(remoteInbound) {
  const values = remoteInbound.map((item) => item.roundTripTime).filter((value) => finite(value) && value >= 0)
  if (!values.length) return null
  return milliseconds(values.reduce((sum, value) => sum + value, 0) / values.length)
}

export function collectCallQuality(stats, previous = {}) {
  const reports = reportsMap(stats)
  const inbound = [...reports.values()].filter((item) => item.type === 'inbound-rtp' && audio(item))
  const outbound = [...reports.values()].filter((item) => item.type === 'outbound-rtp' && audio(item))
  const outboundIds = new Set(outbound.map((item) => item.id))
  const outboundRemoteIds = new Set(outbound.map((item) => item.remoteId).filter(Boolean))
  const remoteInbound = [...reports.values()].filter((item) => {
    const linked = outboundIds.has(item.localId) || outboundRemoteIds.has(item.id)
    const mediaKind = item.kind || item.mediaType
    return item.type === 'remote-inbound-rtp' && linked && item.active !== false && (!mediaKind || mediaKind === 'audio')
  })
  const rtp = [...inbound, ...outbound]
  const routeEvidence = resolveRoute(reports, rtp)
  const route = routeEvidence?.route || null
  const routeChanged = Boolean(previous.routePairId && routeEvidence?.pairId && previous.routePairId !== routeEvidence.pairId)
  const previousInbound = routeChanged ? new Map() : (previous.inbound || new Map())
  const previousRemoteInbound = routeChanged ? new Map() : (previous.remoteInbound || new Map())
  const inboundLoss = intervalMetric(inbound, previousInbound, 'packetsReceived')
  let outboundLoss = intervalMetric(remoteInbound, previousRemoteInbound, 'packetsReceived')
  if (outboundLoss.value === null) {
    const fractions = remoteInbound.map((item) => item.fractionLost).filter((value) => finite(value) && value >= 0 && value <= 1)
    if (fractions.length) outboundLoss = { value: Math.round((fractions.reduce((sum, value) => sum + value, 0) / fractions.length) * 10_000) / 100, windowMs: null }
  }
  let roundTripTimeMs = mediaRoundTrip(remoteInbound)
  let roundTripScope = roundTripTimeMs === null ? '' : 'media-peer'
  if (roundTripTimeMs === null && route) {
    const pairIds = new Set(rtp.map((item) => reports.get(item.transportId)?.selectedCandidatePairId).filter(Boolean))
    if (pairIds.size === 1) {
      roundTripTimeMs = milliseconds(reports.get([...pairIds][0])?.currentRoundTripTime)
      if (roundTripTimeMs !== null) roundTripScope = 'ice-path'
    }
  }
  const valuesAvailable = [inboundLoss.value, outboundLoss.value, currentJitter(inbound, previousInbound), roundTripTimeMs].some((value) => value !== null)
  return {
    sample: {
      status: valuesAvailable ? 'available' : 'collecting',
      inboundLossPercent: inboundLoss.value,
      outboundLossPercent: outboundLoss.value,
      inboundJitterMs: currentJitter(inbound, previousInbound),
      roundTripTimeMs,
      roundTripScope,
      inboundWindowMs: inboundLoss.windowMs,
      outboundWindowMs: outboundLoss.windowMs,
      route,
    },
    next: {
      inbound: nextCounters(inbound, 'packetsReceived'),
      remoteInbound: nextCounters(remoteInbound, 'packetsReceived'),
      routePairId: routeEvidence?.pairId || previous.routePairId || '',
    },
  }
}

function unavailableStatus(peerConnection) {
  const connection = peerConnection.connectionState
  const ice = peerConnection.iceConnectionState
  if (connection === 'disconnected' || ice === 'disconnected') return 'reconnecting'
  if (connection === 'failed' || connection === 'closed' || ice === 'failed' || ice === 'closed') return 'unavailable'
  if (connection === 'new' || connection === 'connecting' || ice === 'new' || ice === 'checking') return 'connecting'
  return ''
}

export function createCallQualitySampler({
  peerConnection,
  onSample,
  onStats = () => {},
  onError = () => {},
  intervalMs = 1_000,
  now = () => Date.now(),
  setTimer = (fn, delay) => setTimeout(fn, delay),
  clearTimer = (timer) => clearTimeout(timer),
}) {
  let active = false
  let inFlight = false
  let timer = null
  let previous = {}
  const schedule = () => {
    if (!active || timer !== null) return
    timer = setTimer(() => { timer = null; void poll() }, intervalMs)
  }
  const poll = async () => {
    if (!active || inFlight) return
    const unavailable = unavailableStatus(peerConnection)
    if (unavailable) {
      previous = {}
      onSample({ status: unavailable, sampledAtMs: now(), inboundLossPercent: null, outboundLossPercent: null, inboundJitterMs: null, roundTripTimeMs: null, roundTripScope: '', inboundWindowMs: null, outboundWindowMs: null, route: null })
      schedule()
      return
    }
    if (typeof peerConnection.getStats !== 'function') {
      onSample({ status: 'unsupported', sampledAtMs: now(), inboundLossPercent: null, outboundLossPercent: null, inboundJitterMs: null, roundTripTimeMs: null, roundTripScope: '', inboundWindowMs: null, outboundWindowMs: null, route: null })
      schedule()
      return
    }
    inFlight = true
    try {
      const stats = await peerConnection.getStats()
      if (!active) return
      onStats(stats)
      const result = collectCallQuality(stats, previous)
      previous = result.next
      onSample({ ...result.sample, sampledAtMs: now() })
    } catch (error) {
      if (active) {
        onError(error)
        onSample({ status: 'unavailable', sampledAtMs: now(), inboundLossPercent: null, outboundLossPercent: null, inboundJitterMs: null, roundTripTimeMs: null, roundTripScope: '', inboundWindowMs: null, outboundWindowMs: null, route: null })
      }
    } finally {
      inFlight = false
      schedule()
    }
  }
  return {
    start() { if (!active) { active = true; void poll() } },
    stop() { active = false; if (timer !== null) clearTimer(timer); timer = null },
    poll,
  }
}
