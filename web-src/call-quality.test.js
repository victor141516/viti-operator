import assert from 'node:assert/strict'
import test from 'node:test'

import { collectCallQuality, createCallQualitySampler } from './call-quality.js'

const report = (...items) => new Map(items.map((item) => [item.id, item]))

test('derives interval inbound loss, jitter, media RTT and a UDP TURN route', () => {
  const first = report(
    { id: 'in-1', type: 'inbound-rtp', kind: 'audio', packetsReceived: 100, packetsLost: 2, jitter: 0.012, timestamp: 1_000, transportId: 'transport' },
    { id: 'out-1', type: 'outbound-rtp', kind: 'audio', packetsSent: 80, timestamp: 1_000, transportId: 'transport', remoteId: 'remote-in-1' },
    { id: 'remote-in-1', type: 'remote-inbound-rtp', localId: 'out-1', packetsReceived: 78, packetsLost: 2, jitter: 0.02, roundTripTime: 0.043, timestamp: 1_000 },
    { id: 'transport', type: 'transport', selectedCandidatePairId: 'pair' },
    { id: 'pair', type: 'candidate-pair', localCandidateId: 'local', remoteCandidateId: 'remote', state: 'succeeded', currentRoundTripTime: 0.031 },
    { id: 'local', type: 'local-candidate', candidateType: 'relay', protocol: 'udp', relayProtocol: 'udp' },
    { id: 'remote', type: 'remote-candidate', candidateType: 'srflx', protocol: 'udp' },
  )
  const baseline = collectCallQuality(first)
  const second = report(
    { id: 'in-1', type: 'inbound-rtp', kind: 'audio', packetsReceived: 198, packetsLost: 4, jitter: 0.009, timestamp: 2_500, transportId: 'transport' },
    { id: 'out-1', type: 'outbound-rtp', kind: 'audio', packetsSent: 180, timestamp: 2_500, transportId: 'transport', remoteId: 'remote-in-1' },
    { id: 'remote-in-1', type: 'remote-inbound-rtp', localId: 'out-1', packetsReceived: 176, packetsLost: 4, jitter: 0.015, roundTripTime: 0.043, timestamp: 2_500 },
    { id: 'transport', type: 'transport', selectedCandidatePairId: 'pair' },
    { id: 'pair', type: 'candidate-pair', localCandidateId: 'local', remoteCandidateId: 'remote', state: 'succeeded', currentRoundTripTime: 0.031 },
    { id: 'local', type: 'local-candidate', candidateType: 'relay', protocol: 'udp', relayProtocol: 'udp' },
    { id: 'remote', type: 'remote-candidate', candidateType: 'srflx', protocol: 'udp' },
  )
  const current = collectCallQuality(second, baseline.next)

  assert.equal(current.sample.inboundLossPercent, 2)
  assert.equal(current.sample.outboundLossPercent, 2)
  assert.equal(current.sample.inboundJitterMs, 9)
  assert.equal(current.sample.roundTripTimeMs, 43)
  assert.equal(current.sample.roundTripScope, 'media-peer')
  assert.equal(current.sample.windowMs, 1_500)
  assert.deepEqual(current.sample.route, {
    mode: 'turn-relay',
    protocol: 'udp',
    localCandidateType: 'relay',
    remoteCandidateType: 'srflx',
  })
})

test('keeps loss unavailable for first, idle, reset, added and removed RTP reports', () => {
  const first = collectCallQuality(report(
    { id: 'a', type: 'inbound-rtp', mediaType: 'audio', packetsReceived: 10, packetsLost: 1, timestamp: 1_000 },
  ))
  assert.equal(first.sample.inboundLossPercent, null)

  const idle = collectCallQuality(report(
    { id: 'a', type: 'inbound-rtp', mediaType: 'audio', packetsReceived: 10, packetsLost: 1, timestamp: 2_000 },
  ), first.next)
  assert.equal(idle.sample.inboundLossPercent, null)

  const reset = collectCallQuality(report(
    { id: 'a', type: 'inbound-rtp', mediaType: 'audio', packetsReceived: 2, packetsLost: 0, timestamp: 3_000 },
    { id: 'b', type: 'inbound-rtp', kind: 'audio', packetsReceived: 50, packetsLost: 5, timestamp: 3_000 },
  ), idle.next)
  assert.equal(reset.sample.inboundLossPercent, null)

  const continued = collectCallQuality(report(
    { id: 'b', type: 'inbound-rtp', kind: 'audio', packetsReceived: 140, packetsLost: 15, timestamp: 5_000 },
  ), reset.next)
  assert.equal(continued.sample.inboundLossPercent, 10)
  assert.equal(continued.sample.windowMs, 2_000)
})

test('aggregates continuing SSRC deltas instead of subtracting lifetime totals', () => {
  const first = collectCallQuality(report(
    { id: 'a', type: 'inbound-rtp', kind: 'audio', packetsReceived: 100, packetsLost: 0, timestamp: 1_000 },
    { id: 'b', type: 'inbound-rtp', kind: 'audio', packetsReceived: 1_000, packetsLost: 100, timestamp: 1_000 },
  ))
  const second = collectCallQuality(report(
    { id: 'a', type: 'inbound-rtp', kind: 'audio', packetsReceived: 190, packetsLost: 10, timestamp: 2_000 },
    { id: 'b', type: 'inbound-rtp', kind: 'audio', packetsReceived: 1_000, packetsLost: 100, timestamp: 2_000 },
  ), first.next)
  assert.equal(second.sample.inboundLossPercent, 10)
})

test('does not turn signed loss corrections or inactive reports into packet loss', () => {
  const negative = collectCallQuality(report(
    { id: 'a', type: 'inbound-rtp', kind: 'audio', packetsReceived: 100, packetsLost: -2, timestamp: 1_000 },
  ))
  const corrected = collectCallQuality(report(
    { id: 'a', type: 'inbound-rtp', kind: 'audio', packetsReceived: 110, packetsLost: 0, timestamp: 2_000 },
    { id: 'inactive', type: 'inbound-rtp', kind: 'audio', active: false, packetsReceived: 1, packetsLost: 1, timestamp: 2_000 },
  ), negative.next)
  assert.equal(corrected.sample.inboundLossPercent, null)
})

test('uses the audio transport selected pair and refuses an ambiguous legacy route', () => {
  const exact = collectCallQuality(report(
    { id: 'in', type: 'inbound-rtp', kind: 'audio', transportId: 'audio-transport', packetsReceived: 1, packetsLost: 0, timestamp: 1 },
    { id: 'audio-transport', type: 'transport', selectedCandidatePairId: 'audio-pair' },
    { id: 'audio-pair', type: 'candidate-pair', localCandidateId: 'audio-local', remoteCandidateId: 'audio-remote' },
    { id: 'video-pair', type: 'candidate-pair', selected: true, localCandidateId: 'video-local', remoteCandidateId: 'video-remote' },
    { id: 'audio-local', type: 'local-candidate', candidateType: 'host', protocol: 'udp' },
    { id: 'audio-remote', type: 'remote-candidate', candidateType: 'srflx', protocol: 'udp' },
    { id: 'video-local', type: 'local-candidate', candidateType: 'relay', protocol: 'tcp' },
    { id: 'video-remote', type: 'remote-candidate', candidateType: 'relay', protocol: 'tcp' },
  ))
  assert.equal(exact.sample.route.mode, 'direct')

  const ambiguous = collectCallQuality(report(
    { id: 'in', type: 'inbound-rtp', kind: 'audio', packetsReceived: 1, packetsLost: 0, timestamp: 1 },
    { id: 'one', type: 'candidate-pair', selected: true, localCandidateId: 'l1', remoteCandidateId: 'r1' },
    { id: 'two', type: 'candidate-pair', selected: true, localCandidateId: 'l2', remoteCandidateId: 'r2' },
  ))
  assert.equal(ambiguous.sample.route, null)
})

test('starts a fresh loss window when the selected ICE pair changes', () => {
  const stats = (pair, received, lost, timestamp) => report(
    { id: 'in', type: 'inbound-rtp', kind: 'audio', transportId: 'transport', packetsReceived: received, packetsLost: lost, timestamp },
    { id: 'transport', type: 'transport', selectedCandidatePairId: pair },
    { id: pair, type: 'candidate-pair', localCandidateId: `local-${pair}`, remoteCandidateId: `remote-${pair}` },
    { id: `local-${pair}`, type: 'local-candidate', candidateType: 'host', protocol: 'udp' },
    { id: `remote-${pair}`, type: 'remote-candidate', candidateType: 'srflx', protocol: 'udp' },
  )
  const first = collectCallQuality(stats('pair-a', 100, 1, 1_000))
  const changed = collectCallQuality(stats('pair-b', 190, 11, 2_000), first.next)
  assert.equal(changed.sample.inboundLossPercent, null)
  const stable = collectCallQuality(stats('pair-b', 280, 21, 3_000), changed.next)
  assert.equal(stable.sample.inboundLossPercent, 10)
})

test('sampler serializes getStats and drops a result after cancellation', async () => {
  let resolveStats
  let calls = 0
  const peer = {
    connectionState: 'connected', iceConnectionState: 'connected',
    getStats() { calls += 1; return new Promise((resolve) => { resolveStats = resolve }) },
  }
  const samples = []
  const scheduled = []
  const sampler = createCallQualitySampler({
    peerConnection: peer,
    onSample: (sample) => samples.push(sample),
    setTimer: (fn) => { scheduled.push(fn); return fn },
    clearTimer: () => {},
  })
  sampler.start()
  sampler.poll()
  assert.equal(calls, 1)
  sampler.stop()
  resolveStats(report())
  await Promise.resolve()
  await Promise.resolve()
  assert.deepEqual(samples, [])
  assert.equal(scheduled.length, 0)
})

test('sampler reports reconnecting without frozen metrics and recovers', async () => {
  const peer = { connectionState: 'disconnected', iceConnectionState: 'disconnected', getStats: async () => report() }
  const samples = []
  const sampler = createCallQualitySampler({
    peerConnection: peer,
    onSample: (sample) => samples.push(sample),
    setTimer: () => 1,
    clearTimer: () => {},
  })
  sampler.start()
  assert.equal(samples.at(-1).status, 'reconnecting')
  assert.equal(samples.at(-1).inboundLossPercent, null)
  peer.connectionState = 'connected'
  peer.iceConnectionState = 'connected'
  await sampler.poll()
  assert.equal(samples.at(-1).status, 'collecting')
})

test('sampler distinguishes unsupported and failed statistics without retaining values', async () => {
  const samples = []
  const unsupported = createCallQualitySampler({
    peerConnection: { connectionState: 'connected', iceConnectionState: 'connected' },
    onSample: (sample) => samples.push(sample), setTimer: () => 1, clearTimer: () => {},
  })
  unsupported.start()
  assert.equal(samples.at(-1).status, 'unsupported')

  let observedError = null
  const failed = createCallQualitySampler({
    peerConnection: { connectionState: 'connected', iceConnectionState: 'connected', getStats: async () => { throw new Error('stats failed') } },
    onSample: (sample) => samples.push(sample), onError: (error) => { observedError = error },
    setTimer: () => 1, clearTimer: () => {},
  })
  failed.start()
  await Promise.resolve()
  await Promise.resolve()
  assert.equal(samples.at(-1).status, 'unavailable')
  assert.equal(samples.at(-1).roundTripTimeMs, null)
  assert.equal(observedError?.message, 'stats failed')
})
