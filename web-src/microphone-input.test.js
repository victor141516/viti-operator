import test from 'node:test'
import assert from 'node:assert/strict'
import { createMicrophoneInputController } from './microphone-input.js'

function track(id) { return { id, kind: 'audio', label: id, enabled: true, stopped: false, stop() { this.stopped = true } } }

function activeFixture({ replaceError = null, muted = false } = {}) {
  const previous = track('old')
  const replacement = track('new')
  const sender = {
    track: previous,
    calls: [],
    async replaceTrack(value) {
      this.calls.push(value)
      if (replaceError) throw replaceError
      this.track = value
    },
  }
  const session = { connection: { getSenders: () => [sender] } }
  const requests = []
  const controller = createMicrophoneInputController({
    mediaDevices: { async getUserMedia(constraints) { requests.push(constraints); return { getAudioTracks: () => [replacement] } } },
    getSession: () => session,
    isMuted: () => muted,
  })
  return { controller, previous, replacement, sender, requests }
}

test('uses the selected input constraint on the next media acquisition', async () => {
  const controller = createMicrophoneInputController({ getSession: () => null })
  await controller.setDevice('preferred-mic')
  assert.deepEqual(controller.constraint(), { deviceId: { exact: 'preferred-mic' } })
  await controller.setDevice('')
  assert.equal(controller.constraint(), true)
})

test('atomically replaces the active sender track and stops the previous track', async () => {
  const fixture = activeFixture()
  await fixture.controller.setDevice('preferred-mic')
  assert.deepEqual(fixture.requests, [{ audio: { deviceId: { exact: 'preferred-mic' } }, video: false }])
  assert.deepEqual(fixture.sender.calls, [fixture.replacement])
  assert.equal(fixture.previous.stopped, true)
  assert.equal(fixture.replacement.stopped, false)
  assert.equal(fixture.controller.deviceId, 'preferred-mic')
})

test('keeps a replacement track muted when switching during a muted call', async () => {
  const fixture = activeFixture({ muted: true })
  await fixture.controller.setDevice('quiet-mic')
  assert.equal(fixture.replacement.enabled, false)
})

test('a failed replacement stops the candidate and preserves the previous preference and track', async () => {
  const fixture = activeFixture({ replaceError: new Error('replace failed') })
  await assert.rejects(fixture.controller.setDevice('broken-mic'), /replace failed/)
  assert.equal(fixture.controller.deviceId, '')
  assert.equal(fixture.previous.stopped, false)
  assert.equal(fixture.replacement.stopped, true)
  assert.equal(fixture.sender.track, fixture.previous)
})

test('release stops the controller-owned replacement track', async () => {
  const fixture = activeFixture()
  await fixture.controller.setDevice('preferred-mic')
  fixture.controller.release()
  assert.equal(fixture.replacement.stopped, true)
})

test('a call ending while replaceTrack is pending cannot leak the candidate into later state', async () => {
  let markReplacementStarted
  const replacementStarted = new Promise((resolve) => { markReplacementStarted = resolve })
  let finishReplacement
  const replacementGate = new Promise((resolve) => { finishReplacement = resolve })
  const previous = track('old')
  const replacement = track('new')
  const sender = { track: previous, async replaceTrack(value) { this.track = value; markReplacementStarted(); await replacementGate } }
  let session = { connection: { getSenders: () => [sender] } }
  const controller = createMicrophoneInputController({
    mediaDevices: { async getUserMedia() { return { getAudioTracks: () => [replacement] } } },
    getSession: () => session,
  })
  const switching = controller.setDevice('next-call-mic')
  await replacementStarted
  session = null
  controller.release()
  finishReplacement()
  await switching
  assert.equal(replacement.stopped, true)
  assert.equal(controller.deviceId, 'next-call-mic')
})

test('a replacement call starting mid-switch rejects instead of claiming an unapplied preference', async () => {
  let markReplacementStarted
  const replacementStarted = new Promise((resolve) => { markReplacementStarted = resolve })
  let finishReplacement
  const replacementGate = new Promise((resolve) => { finishReplacement = resolve })
  const previous = track('old')
  const replacement = track('candidate')
  const sender = { track: previous, async replaceTrack(value) { this.track = value; markReplacementStarted(); await replacementGate } }
  const original = { connection: { getSenders: () => [sender] } }
  const next = { connection: { getSenders: () => [] } }
  let session = original
  const controller = createMicrophoneInputController({
    mediaDevices: { async getUserMedia() { return { getAudioTracks: () => [replacement] } } },
    getSession: () => session,
  })
  const switching = controller.setDevice('new-preference')
  await replacementStarted
  session = next
  finishReplacement()
  await assert.rejects(switching, /call changed while switching microphones/i)
  assert.equal(replacement.stopped, true)
  assert.equal(controller.deviceId, '')
})
