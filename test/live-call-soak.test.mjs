import assert from 'node:assert/strict'
import test from 'node:test'

import { LiveCallSoakTracker } from './live-call-soak-core.mjs'

const sample = (seconds, overrides = {}) => ({
  timestamp: new Date(seconds * 1000).toISOString(),
  snapshot_ok: true,
  active_channels: 4,
  charon: true,
  python: false,
  tunnel: true,
  ims: true,
  ike_ids: [1],
  child_ids: [10],
  media_channels: 2,
  media_rx_packets: seconds * 50,
  media_tx_packets: seconds * 50,
  media_streams: [{ id: 'client', rx_packets: seconds * 50, tx_packets: seconds * 50 }],
  ...overrides,
})

test('accepts one call only after the required natural SA transitions and post-call window', () => {
  const tracker = new LiveCallSoakTracker({
    minCallSeconds: 100,
    minIkeRenewals: 2,
    minChildRenewals: 2,
    postCallSeconds: 10,
  })
  tracker.observe(sample(0, { active_channels: 0 }))
  tracker.observe(sample(10))
  tracker.observe(sample(50, { ike_ids: [2], child_ids: [11] }))
  tracker.observe(sample(90, { ike_ids: [3], child_ids: [12] }))
  tracker.observe(sample(115))
  tracker.observe(sample(120, { active_channels: 0 }))
  tracker.observe(sample(130, { active_channels: 0 }))

  assert.deepEqual(tracker.summary(), {
    accepted: true,
    phase: 'complete',
    fatal_reason: null,
    call_detected: true,
    call_duration_seconds: 110,
    active_samples: 4,
    unhealthy_samples: 0,
    observed_ike_renewals: 2,
    observed_child_renewals: 2,
    media_rx_progress_samples: 3,
    media_tx_progress_samples: 3,
    maximum_observed_media_rx_stall_seconds: 0,
    maximum_observed_media_tx_stall_seconds: 0,
    requirements: {
      minimum_call_seconds: 100,
      minimum_ike_renewals: 2,
      minimum_child_renewals: 2,
      post_call_seconds: 10,
      maximum_media_stall_seconds: 15,
    },
  })
})

test('does not treat a failed snapshot as a remote hangup', () => {
  const tracker = new LiveCallSoakTracker({ minCallSeconds: 10, minIkeRenewals: 0, postCallSeconds: 1 })
  tracker.observe(sample(1))
  tracker.observe(sample(5, { snapshot_ok: false, active_channels: 0 }))
  tracker.observe(sample(11))
  tracker.observe(sample(12, { active_channels: 0 }))
  tracker.observe(sample(13, { active_channels: 0 }))

  const summary = tracker.summary()
  assert.equal(summary.call_duration_seconds, 11)
  assert.equal(summary.unhealthy_samples, 1)
  assert.equal(summary.accepted, false)
})

test('rejects infrastructure loss during the active call', () => {
  const tracker = new LiveCallSoakTracker({ minCallSeconds: 1, minIkeRenewals: 0, postCallSeconds: 0 })
  tracker.observe(sample(1))
  tracker.observe(sample(2, { ims: false }))
  tracker.observe(sample(3, { active_channels: 0 }))
  tracker.observe(sample(4, { active_channels: 0 }))

  assert.equal(tracker.summary().unhealthy_samples, 1)
  assert.equal(tracker.summary().accepted, false)
})

test('rejects a second call during the post-call observation window', () => {
  const tracker = new LiveCallSoakTracker({ minCallSeconds: 1, minIkeRenewals: 0, postCallSeconds: 10 })
  tracker.observe(sample(1))
  tracker.observe(sample(3, { active_channels: 0 }))
  tracker.observe(sample(5))

  assert.equal(tracker.summary().fatal_reason, 'another call started during the post-call observation window')
  assert.equal(tracker.summary().accepted, false)
})

test('counts each replacement SA once while old and new generations overlap', () => {
  const tracker = new LiveCallSoakTracker({ minCallSeconds: 2, minIkeRenewals: 1, minChildRenewals: 1, postCallSeconds: 0 })
  tracker.observe(sample(1))
  tracker.observe(sample(2, { ike_ids: [1, 2], child_ids: [10, 11] }))
  tracker.observe(sample(3, { ike_ids: [2], child_ids: [11] }))
  tracker.observe(sample(4, { active_channels: 0 }))
  tracker.observe(sample(5, { active_channels: 0 }))

  const summary = tracker.summary()
  assert.equal(summary.observed_ike_renewals, 1)
  assert.equal(summary.observed_child_renewals, 1)
  assert.equal(summary.accepted, true)
})

test('rejects a call whose RTP packet counters stop advancing', () => {
  const tracker = new LiveCallSoakTracker({
    minCallSeconds: 5,
    minIkeRenewals: 0,
    minChildRenewals: 0,
    postCallSeconds: 0,
    maxMediaStallSeconds: 5,
  })
  tracker.observe(sample(0, { active_channels: 0 }))
  const frozen = { media_rx_packets: 100, media_tx_packets: 200, media_streams: [{ id: 'client', rx_packets: 100, tx_packets: 200 }] }
  tracker.observe(sample(1, frozen))
  tracker.observe(sample(4, frozen))
  tracker.observe(sample(7, frozen))
  tracker.observe(sample(8, { active_channels: 0 }))
  tracker.observe(sample(9, { active_channels: 0 }))

  const summary = tracker.summary()
  assert.equal(summary.accepted, false)
  assert.equal(summary.maximum_observed_media_rx_stall_seconds, 6)
  assert.equal(summary.maximum_observed_media_tx_stall_seconds, 6)
  assert.deepEqual(tracker.unhealthySamples.at(-1).reasons, ['media_rx_stalled', 'media_tx_stalled'])
})

test('rejects an active call without observable PJSIP media channels', () => {
  const tracker = new LiveCallSoakTracker({ minCallSeconds: 1, minIkeRenewals: 0, minChildRenewals: 0, postCallSeconds: 0 })
  tracker.observe(sample(1, { media_channels: 0, media_streams: [] }))
  tracker.observe(sample(2, { active_channels: 0 }))
  tracker.observe(sample(3, { active_channels: 0 }))

  assert.equal(tracker.summary().accepted, false)
  assert.deepEqual(tracker.unhealthySamples[0].reasons, ['media_channels_absent'])
})

test('does not let a healthy stream hide another stream that stalls', () => {
  const tracker = new LiveCallSoakTracker({
    minCallSeconds: 5,
    minIkeRenewals: 0,
    minChildRenewals: 0,
    postCallSeconds: 0,
    maxMediaStallSeconds: 5,
  })
  const streams = healthy => ([
    { id: 'stalled', rx_packets: 10, tx_packets: 20 },
    { id: 'healthy', rx_packets: healthy, tx_packets: healthy },
  ])
  tracker.observe(sample(1, { media_streams: streams(100) }))
  tracker.observe(sample(4, { media_streams: streams(200), media_rx_packets: 210, media_tx_packets: 220 }))
  tracker.observe(sample(7, { media_streams: streams(300), media_rx_packets: 310, media_tx_packets: 320 }))
  tracker.observe(sample(8, { active_channels: 0 }))
  tracker.observe(sample(9, { active_channels: 0 }))

  assert.equal(tracker.summary().accepted, false)
  assert.deepEqual(tracker.unhealthySamples.at(-1).reasons, ['media_rx_stalled', 'media_tx_stalled'])
})

test('rejects a media stream disappearing while the call remains active', () => {
  const tracker = new LiveCallSoakTracker({ minCallSeconds: 1, minIkeRenewals: 0, minChildRenewals: 0, postCallSeconds: 0 })
  tracker.observe(sample(1, { media_streams: [
    { id: 'carrier', rx_packets: 10, tx_packets: 10 },
    { id: 'client', rx_packets: 10, tx_packets: 10 },
  ] }))
  tracker.observe(sample(2, { media_streams: [{ id: 'carrier', rx_packets: 20, tx_packets: 20 }] }))
  tracker.observe(sample(3, { active_channels: 0 }))
  tracker.observe(sample(4, { active_channels: 0 }))

  assert.equal(tracker.summary().accepted, false)
  assert.deepEqual(tracker.unhealthySamples[0].reasons, ['media_stream_removed'])
})
