import test from 'node:test'
import assert from 'node:assert/strict'
import { createMicrophoneMuteController } from './microphone-mute.js'

function fakeSession() {
  const audio = { kind: 'audio', enabled: true }
  const video = { kind: 'video', enabled: true }
  return {
    audio,
    video,
    muteCalls: 0,
    unmuteCalls: 0,
    mute(options) { assert.deepEqual(options, { audio: true }); this.muteCalls++; audio.enabled = false },
    unmute(options) { assert.deepEqual(options, { audio: true }); this.unmuteCalls++; audio.enabled = true },
    connection: { getSenders: () => [{ track: audio }, { track: video }, { track: null }] },
  }
}

test('mute disables only the outbound audio track and reports state once', () => {
  const changes = []
  const controller = createMicrophoneMuteController((value) => changes.push(value))
  const session = fakeSession()

  assert.equal(controller.set(true, session), true)
  assert.equal(session.audio.enabled, false)
  assert.equal(session.video.enabled, true)
  assert.equal(session.muteCalls, 1)
  assert.deepEqual(changes, [true])

  controller.set(true, session)
  assert.deepEqual(changes, [true])
})

test('unmute restores outbound audio without changing other tracks', () => {
  const controller = createMicrophoneMuteController()
  const session = fakeSession()
  controller.set(true, session)
  controller.set(false, session)

  assert.equal(session.audio.enabled, true)
  assert.equal(session.video.enabled, true)
  assert.equal(session.unmuteCalls, 1)
})

test('muted state is applied to replacement media in the same call', () => {
  const controller = createMicrophoneMuteController()
  const original = fakeSession()
  const replacement = fakeSession()

  controller.set(true, original)
  // Model JsSIP's no-op when its internal audio-muted flag is already true:
  // the replacement sender starts enabled and our fallback must mute it.
  replacement.mute = function (options) {
    assert.deepEqual(options, { audio: true })
    this.muteCalls++
  }
  controller.apply(replacement)

  assert.equal(controller.muted, true)
  assert.equal(replacement.audio.enabled, false)
  assert.equal(replacement.muteCalls, 1)
})

test('reset starts the next call unmuted', () => {
  const changes = []
  const controller = createMicrophoneMuteController((value) => changes.push(value))
  const session = fakeSession()
  controller.set(true, session)
  controller.reset(session)

  assert.equal(controller.muted, false)
  assert.equal(session.audio.enabled, true)
  assert.deepEqual(changes, [true, false])
})
