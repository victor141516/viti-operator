import test from 'node:test'
import assert from 'node:assert/strict'
import { audioLevelFromTimeDomain, createAudioLevelMeter } from './audio-level-meter.js'

test('maps silence to zero and louder waveforms to larger levels', () => {
  const silence = new Uint8Array(64).fill(128)
  const quiet = Uint8Array.from({ length: 64 }, (_, index) => 128 + (index % 2 ? 3 : -3))
  const loud = Uint8Array.from({ length: 64 }, (_, index) => 128 + (index % 2 ? 80 : -80))
  assert.equal(audioLevelFromTimeDomain(silence), 0)
  assert.ok(audioLevelFromTimeDomain(quiet) > 0)
  assert.ok(audioLevelFromTimeDomain(loud) > audioLevelFromTimeDomain(quiet))
  assert.ok(audioLevelFromTimeDomain(loud) <= 1)
})

test('meter taps a stream without connecting it to audio output and cleans up', () => {
  let sample
  let cleared = false
  const levels = []
  const source = { connectedTo: null, disconnected: false, connect(value) { this.connectedTo = value }, disconnect() { this.disconnected = true } }
  const analyser = {
    fftSize: 0,
    smoothingTimeConstant: 0,
    getByteTimeDomainData(buffer) { buffer.fill(128); buffer[0] = 208 },
  }
  const context = {
    destination: { forbidden: true },
    createAnalyser: () => analyser,
    createMediaStreamSource: () => source,
    resume: async () => {},
    close: async () => {},
  }
  const stream = { id: 'remote' }
  const meter = createAudioLevelMeter({
    AudioContextClass: class { constructor() { return context } },
    setTimer: (callback) => { sample = callback; return 7 },
    clearTimer: (value) => { assert.equal(value, 7); cleared = true },
    onLevel: (value) => levels.push(value),
  })
  assert.equal(meter.attach(stream), true)
  sample()
  assert.equal(source.connectedTo, analyser)
  assert.notEqual(source.connectedTo, context.destination)
  assert.ok(levels.at(-1) > 0)
  meter.stop()
  assert.equal(cleared, true)
  assert.equal(source.disconnected, true)
  assert.equal(levels.at(-1), 0)
})
