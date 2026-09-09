import test from 'node:test'
import assert from 'node:assert/strict'
import { createAudioPreferencesController, resolveAudioDevice } from './audio-preferences.js'

function device(kind, deviceId, groupId, label) { return { kind, deviceId, groupId, label } }

function fakeStorage(initial = null) {
  const values = new Map(initial ? [['viti-audio-preferences-v1', JSON.stringify(initial)]] : [])
  return { getItem: (key) => values.get(key) || null, setItem: (key, value) => values.set(key, value), read: () => JSON.parse(values.get('viti-audio-preferences-v1') || '{}') }
}

function fakeMediaDevices(initial) {
  let devices = initial
  const listeners = new Set()
  return {
    enumerateDevices: async () => devices,
    getUserMedia: async () => ({ getTracks: () => [{ stop() {} }] }),
    addEventListener(name, listener) { if (name === 'devicechange') listeners.add(listener) },
    removeEventListener(name, listener) { if (name === 'devicechange') listeners.delete(listener) },
    change(next) { devices = next; for (const listener of listeners) listener() },
  }
}

function fakeAudioElement() {
  return { sinkId: '', calls: [], async setSinkId(value) { this.calls.push(value); this.sinkId = value } }
}

test('resolves a changed device id through a unique physical group', () => {
  const result = resolveAudioDevice(
    [device('audioinput', 'new-id', 'headset-group', 'Bluetooth headset')],
    'audioinput',
    { deviceId: 'old-id', groupId: 'headset-group', label: 'Bluetooth headset' },
  )
  assert.equal(result.available, true)
  assert.equal(result.deviceId, 'new-id')
  assert.equal(result.match, 'group-id')
  assert.equal(result.remapped, true)
})

test('does not guess between duplicate label matches', () => {
  const result = resolveAudioDevice([
    device('audioinput', 'one', '', 'USB Audio'),
    device('audioinput', 'two', '', 'USB Audio'),
  ], 'audioinput', { deviceId: 'gone', groupId: '', label: 'USB Audio' })
  assert.equal(result.available, false)
  assert.equal(result.deviceId, '')
})

test('does not offer anonymous placeholder devices as selectable choices before permission', async () => {
  const controller = createAudioPreferencesController({
    mediaDevices: fakeMediaDevices([device('audioinput', '', '', ''), device('audiooutput', '', '', '')]),
    storage: fakeStorage(),
    audioElement: fakeAudioElement(),
  })
  await controller.start()
  assert.deepEqual(controller.snapshot().inputs, [])
  assert.deepEqual(controller.snapshot().outputs, [])
  assert.equal(controller.snapshot().permissionNeeded, true)
})

test('restores persisted input and output preferences', async () => {
  const mediaDevices = fakeMediaDevices([
    device('audioinput', 'mic-1', 'mic-group', 'Desk microphone'),
    device('audiooutput', 'speaker-1', 'speaker-group', 'Desk speakers'),
  ])
  const storage = fakeStorage({
    audioinput: { deviceId: 'mic-1', groupId: 'mic-group', label: 'Desk microphone' },
    audiooutput: { deviceId: 'speaker-1', groupId: 'speaker-group', label: 'Desk speakers' },
  })
  const inputs = []
  const audioElement = fakeAudioElement()
  const controller = createAudioPreferencesController({ mediaDevices, storage, audioElement, applyInput: async (id) => inputs.push(id) })
  await controller.start()
  assert.deepEqual(inputs, ['mic-1'])
  assert.deepEqual(audioElement.calls, ['speaker-1'])
  assert.equal(controller.snapshot().audioinput.selectedId, 'mic-1')
  assert.equal(controller.snapshot().audiooutput.selectedId, 'speaker-1')
})

test('falls back on Bluetooth departure and restores it when it returns with a new id', async () => {
  const bluetoothMic = device('audioinput', 'bt-old', 'bt-group', 'Bluetooth headset')
  const mediaDevices = fakeMediaDevices([bluetoothMic])
  const storage = fakeStorage({ audioinput: { deviceId: 'bt-old', groupId: 'bt-group', label: 'Bluetooth headset' }, audiooutput: null })
  const inputs = []
  const controller = createAudioPreferencesController({ mediaDevices, storage, audioElement: fakeAudioElement(), applyInput: async (id) => inputs.push(id) })
  await controller.start()

  mediaDevices.change([])
  await controller.whenIdle()
  assert.equal(controller.snapshot().audioinput.selectedId, '')
  assert.match(controller.snapshot().audioinput.detail, /unavailable; using the system default/i)
  assert.equal(storage.read().audioinput.deviceId, 'bt-old', 'the desired device remains persisted while absent')

  mediaDevices.change([device('audioinput', 'bt-new', 'bt-group', 'Bluetooth headset')])
  await controller.whenIdle()
  assert.deepEqual(inputs, ['bt-old', '', 'bt-new'])
  assert.equal(controller.snapshot().audioinput.selectedId, 'bt-new')
  assert.equal(storage.read().audioinput.deviceId, 'bt-new')
})

test('moves Bluetooth speaker output to default while absent and restores it on arrival', async () => {
  const speaker = device('audiooutput', 'speaker-old', 'speaker-group', 'Bluetooth headset')
  const mediaDevices = fakeMediaDevices([speaker])
  const storage = fakeStorage({ audioinput: null, audiooutput: { deviceId: 'speaker-old', groupId: 'speaker-group', label: 'Bluetooth headset' } })
  const audioElement = fakeAudioElement()
  const controller = createAudioPreferencesController({ mediaDevices, storage, audioElement })
  await controller.start()

  mediaDevices.change([])
  await controller.whenIdle()
  assert.equal(controller.snapshot().audiooutput.selectedId, '')
  assert.equal(storage.read().audiooutput.deviceId, 'speaker-old')

  mediaDevices.change([device('audiooutput', 'speaker-new', 'speaker-group', 'Bluetooth headset')])
  await controller.whenIdle()
  assert.deepEqual(audioElement.calls, ['speaker-old', '', 'speaker-new'])
  assert.equal(controller.snapshot().audiooutput.selectedId, 'speaker-new')
})

test('a failed live microphone switch preserves the previous effective and persisted choice', async () => {
  const mediaDevices = fakeMediaDevices([
    device('audioinput', 'mic-1', 'one', 'Working microphone'),
    device('audioinput', 'mic-2', 'two', 'Broken microphone'),
  ])
  const storage = fakeStorage({ audioinput: { deviceId: 'mic-1', groupId: 'one', label: 'Working microphone' }, audiooutput: null })
  const controller = createAudioPreferencesController({
    mediaDevices,
    storage,
    audioElement: fakeAudioElement(),
    applyInput: async (id) => { if (id === 'mic-2') throw new Error('replaceTrack failed') },
  })
  await controller.start()
  await assert.rejects(controller.select('audioinput', 'mic-2'), /replaceTrack failed/)
  assert.equal(controller.snapshot().audioinput.selectedId, 'mic-1')
  assert.equal(storage.read().audioinput.deviceId, 'mic-1')
})

test('serialized device changes remain busy until the last queued operation finishes', async () => {
  let releaseFirst
  const first = new Promise((resolve) => { releaseFirst = resolve })
  let releaseSecond
  const second = new Promise((resolve) => { releaseSecond = resolve })
  let calls = 0
  const mediaDevices = fakeMediaDevices([device('audioinput', 'mic-1', 'one', 'Microphone')])
  mediaDevices.enumerateDevices = async () => { calls += 1; await (calls === 1 ? first : second); return [device('audioinput', 'mic-1', 'one', 'Microphone')] }
  const snapshots = []
  const controller = createAudioPreferencesController({ mediaDevices, storage: fakeStorage(), audioElement: fakeAudioElement(), onChange: (value) => snapshots.push(value) })
  const starting = controller.start()
  const refreshing = controller.refresh()
  assert.equal(controller.snapshot().busy, true)
  releaseFirst()
  await starting
  assert.equal(controller.snapshot().busy, true, 'the queued refresh is still pending')
  releaseSecond()
  await refreshing
  assert.equal(controller.snapshot().busy, false)
  assert.equal(snapshots.at(-1).busy, false)
})

test('explains browsers without programmatic speaker selection', async () => {
  const controller = createAudioPreferencesController({
    mediaDevices: fakeMediaDevices([device('audiooutput', 'speaker-1', 'speaker', 'Speakers')]),
    storage: fakeStorage(),
    audioElement: {},
  })
  await controller.start()
  assert.equal(controller.snapshot().outputSupported, false)
  assert.match(controller.snapshot().audiooutput.detail, /browser or operating-system audio controls/i)
})
