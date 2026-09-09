import test from 'node:test'
import assert from 'node:assert/strict'
import { recordingTrackPresence } from './recording-presence.js'

test('late participant is inactive until its real offset', () => {
  const track = { offset_ms: 5_000, duration_ms: 10_000 }
  assert.equal(recordingTrackPresence(track, 4_999).phase, 'before')
  assert.equal(recordingTrackPresence(track, 5_000).phase, 'active')
})

test('short participant becomes inactive at its real end', () => {
  const track = { offset_ms: 5_000, duration_ms: 2_000 }
  assert.equal(recordingTrackPresence(track, 6_999).phase, 'active')
  assert.equal(recordingTrackPresence(track, 7_000).phase, 'after')
})

test('missing duration remains active after the known start', () => {
  const track = { offset_ms: 1_000, duration_ms: 0 }
  assert.equal(recordingTrackPresence(track, 20_000).phase, 'active')
})
