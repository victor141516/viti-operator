export const AUDIO_PREFERENCES_STORAGE_KEY = 'viti-audio-preferences-v1'

const RESERVED_DEVICE_IDS = new Set(['default', 'communications'])
const KIND_LABELS = {
  audioinput: { singular: 'microphone', title: 'Microphone' },
  audiooutput: { singular: 'speaker', title: 'Speaker' },
}

function cleanPreference(value) {
  if (!value || typeof value !== 'object') return null
  const deviceId = typeof value.deviceId === 'string' ? value.deviceId : ''
  const groupId = typeof value.groupId === 'string' ? value.groupId : ''
  const label = typeof value.label === 'string' ? value.label : ''
  return deviceId || groupId || label ? { deviceId, groupId, label } : null
}

function cleanStoredPreferences(value) {
  if (!value || typeof value !== 'object') return { audioinput: null, audiooutput: null }
  return {
    audioinput: cleanPreference(value.audioinput),
    audiooutput: cleanPreference(value.audiooutput),
  }
}

function normalizedLabel(value) {
  return String(value || '').trim().toLocaleLowerCase()
}

function selectableDevices(devices, kind) {
  return (devices || []).filter((device) => device?.kind === kind && Boolean(device.deviceId) && !RESERVED_DEVICE_IDS.has(device.deviceId))
}

function preferenceForDevice(device) {
  if (!device) return null
  return { deviceId: device.deviceId || '', groupId: device.groupId || '', label: device.label || '' }
}

export function resolveAudioDevice(devices, kind, preference) {
  const wanted = cleanPreference(preference)
  if (!wanted) return { available: true, device: null, deviceId: '', remapped: false, match: 'default' }
  const candidates = selectableDevices(devices, kind)
  let device = candidates.find((candidate) => candidate.deviceId && candidate.deviceId === wanted.deviceId)
  let match = device ? 'device-id' : ''
  if (!device && wanted.groupId) {
    const matches = candidates.filter((candidate) => candidate.groupId && candidate.groupId === wanted.groupId)
    if (matches.length === 1) { device = matches[0]; match = 'group-id' }
  }
  if (!device && wanted.label) {
    const label = normalizedLabel(wanted.label)
    const matches = candidates.filter((candidate) => normalizedLabel(candidate.label) === label)
    if (matches.length === 1) { device = matches[0]; match = 'label' }
  }
  if (!device) return { available: false, device: null, deviceId: '', remapped: false, match: 'unavailable' }
  return {
    available: true,
    device,
    deviceId: device.deviceId || '',
    remapped: device.deviceId !== wanted.deviceId || device.groupId !== wanted.groupId || device.label !== wanted.label,
    match,
  }
}

function readPreferences(storage, storageKey) {
  try {
    const raw = storage?.getItem?.(storageKey)
    return raw ? cleanStoredPreferences(JSON.parse(raw)) : cleanStoredPreferences(null)
  } catch {
    return cleanStoredPreferences(null)
  }
}

function writePreferences(storage, storageKey, preferences) {
  try { storage?.setItem?.(storageKey, JSON.stringify(preferences)) } catch {}
}

function displayDevices(devices, kind) {
  const noun = KIND_LABELS[kind]?.title || 'Device'
  return selectableDevices(devices, kind).map((device, index) => ({
    deviceId: device.deviceId || '',
    groupId: device.groupId || '',
    label: device.label || `${noun} ${index + 1}`,
  }))
}

export function createAudioPreferencesController({
  mediaDevices = globalThis.navigator?.mediaDevices,
  audioElement = null,
  storage = globalThis.localStorage,
  storageKey = AUDIO_PREFERENCES_STORAGE_KEY,
  applyInput = async () => {},
  onChange = () => {},
} = {}) {
  let preferences = readPreferences(storage, storageKey)
  let devices = []
  let effective = { audioinput: null, audiooutput: null }
  let details = { audioinput: '', audiooutput: '' }
  let errors = { audioinput: '', audiooutput: '', permission: '' }
  let busy = false
  let disposed = false
  let started = false
  let pendingOperations = 0
  let queue = Promise.resolve()
  const canEnumerate = Boolean(mediaDevices?.enumerateDevices)
  const inputSupported = Boolean(canEnumerate && mediaDevices?.getUserMedia)
  const outputSupported = Boolean(canEnumerate && typeof audioElement?.setSinkId === 'function')
  const outputPickerSupported = Boolean(outputSupported && typeof mediaDevices?.selectAudioOutput === 'function')

  const labelsAvailable = () => devices.some((device) => device.kind.startsWith('audio') && Boolean(device.deviceId) && !RESERVED_DEVICE_IDS.has(device.deviceId) && Boolean(device.label))
  const snapshot = () => ({
    busy,
    inputSupported,
    outputSupported,
    outputPickerSupported,
    permissionNeeded: inputSupported && !labelsAvailable(),
    permissionError: errors.permission,
    inputs: displayDevices(devices, 'audioinput'),
    outputs: displayDevices(devices, 'audiooutput'),
    audioinput: {
      selectedId: effective.audioinput || '',
      preferred: preferences.audioinput ? { ...preferences.audioinput } : null,
      detail: details.audioinput,
      error: errors.audioinput,
    },
    audiooutput: {
      selectedId: effective.audiooutput || '',
      preferred: preferences.audiooutput ? { ...preferences.audiooutput } : null,
      detail: details.audiooutput,
      error: errors.audiooutput,
    },
  })
  const publish = () => { if (!disposed) onChange(snapshot()) }
  const persist = () => writePreferences(storage, storageKey, preferences)

  const applyDevice = async (kind, deviceId) => {
    if (kind === 'audioinput') {
      if (!inputSupported) throw new Error('Microphone selection is not supported by this browser.')
      await applyInput(deviceId)
      return
    }
    if (kind === 'audiooutput') {
      if (!outputSupported) throw new Error('Speaker selection is not supported by this browser.')
      await audioElement.setSinkId(deviceId)
      return
    }
    throw new Error(`Unsupported audio device kind: ${kind}`)
  }

  const describeResolution = (kind, resolution) => {
    const noun = KIND_LABELS[kind].singular
    if (!resolution.available) {
      const label = preferences[kind]?.label || `preferred ${noun}`
      if (kind === 'audiooutput' && outputPickerSupported) return `The speaker “${label}” is unavailable or not authorized; using the system default. Reconnect it or choose a speaker.`
      return `The ${noun} “${label}” is unavailable; using the system default. It will be restored if it reconnects.`
    }
    return resolution.device ? `Using ${resolution.device.label || `the selected ${noun}`}.` : `Using the system default ${noun}.`
  }

  const refreshNow = async () => {
    if (!mediaDevices?.enumerateDevices) {
      details.audioinput = 'This browser cannot list microphones.'
      details.audiooutput = 'This browser cannot list speakers.'
      publish()
      return snapshot()
    }
    try {
      devices = [...await mediaDevices.enumerateDevices()]
      errors.permission = ''
    } catch (error) {
      errors.permission = error?.message || 'Audio devices could not be listed.'
      publish()
      return snapshot()
    }
    for (const kind of ['audioinput', 'audiooutput']) {
      if (kind === 'audioinput' && !inputSupported) {
        details[kind] = 'Microphone selection is not supported by this browser.'
        continue
      }
      if (kind === 'audiooutput' && !outputSupported) {
        details[kind] = 'This browser does not allow websites to choose a speaker. Use the browser or operating-system audio controls.'
        continue
      }
      const resolution = resolveAudioDevice(devices, kind, preferences[kind])
      const nextId = resolution.deviceId
      errors[kind] = ''
      if (effective[kind] !== nextId) {
        try {
          await applyDevice(kind, nextId)
          effective[kind] = nextId
        } catch (error) {
          errors[kind] = error?.message || `The ${KIND_LABELS[kind].singular} could not be changed.`
          continue
        }
      }
      if (resolution.device && resolution.remapped) {
        preferences = { ...preferences, [kind]: preferenceForDevice(resolution.device) }
        persist()
      }
      details[kind] = describeResolution(kind, resolution)
    }
    publish()
    return snapshot()
  }

  const enqueue = (operation) => {
    pendingOperations += 1
    busy = pendingOperations > 0
    publish()
    const result = queue.catch(() => {}).then(operation)
    const tracked = result.finally(() => { pendingOperations -= 1; busy = pendingOperations > 0; publish() })
    queue = tracked.catch(() => {})
    return tracked
  }

  const handleDeviceChange = () => { enqueue(refreshNow).catch(() => {}) }

  return {
    snapshot,
    start() {
      if (started) return queue
      started = true
      mediaDevices?.addEventListener?.('devicechange', handleDeviceChange)
      return enqueue(refreshNow)
    },
    refresh() { return enqueue(refreshNow) },
    requestPermission() {
      return enqueue(async () => {
        if (!inputSupported) throw new Error('Microphone access is not supported by this browser.')
        let stream
        try {
          stream = await mediaDevices.getUserMedia({ audio: true, video: false })
          errors.permission = ''
        } catch (error) {
          errors.permission = error?.name === 'NotAllowedError'
            ? 'Microphone access was not allowed. Device names and selection remain limited.'
            : (error?.message || 'Microphone access failed.')
          publish()
          throw error
        } finally {
          for (const track of stream?.getTracks?.() || []) track.stop()
        }
        return refreshNow()
      })
    },
    chooseOutput() {
      if (!outputPickerSupported) return Promise.reject(new Error('This browser does not provide a speaker picker.'))
      let selection
      try {
        // This must be invoked synchronously from the click handler so the
        // browser's transient user activation is still valid.
        selection = Promise.resolve(mediaDevices.selectAudioOutput()).then(
          (device) => ({ device }),
          (error) => ({ error }),
        )
      } catch (error) {
        errors = { ...errors, audiooutput: error?.message || 'The speaker picker could not be opened.' }
        publish()
        return Promise.reject(error)
      }
      return enqueue(async () => {
        let selected
        try {
          const outcome = await selection
          if (outcome.error) throw outcome.error
          selected = outcome.device
          if (!selected?.deviceId) throw new Error('The browser did not return a usable speaker.')
          await applyDevice('audiooutput', selected.deviceId)
        } catch (error) {
          errors = { ...errors, audiooutput: error?.message || 'The speaker could not be selected.' }
          publish()
          throw error
        }
        try {
          devices = [...await mediaDevices.enumerateDevices()]
        } catch {}
        if (!devices.some((device) => device.kind === 'audiooutput' && device.deviceId === selected.deviceId)) devices.push(selected)
        effective = { ...effective, audiooutput: selected.deviceId }
        preferences = { ...preferences, audiooutput: preferenceForDevice(selected) }
        errors = { ...errors, audiooutput: '' }
        details = { ...details, audiooutput: `Using ${selected.label || 'the selected speaker'}.` }
        persist()
        publish()
        return snapshot()
      })
    },
    select(kind, deviceId = '') {
      return enqueue(async () => {
        if (!KIND_LABELS[kind]) throw new Error(`Unsupported audio device kind: ${kind}`)
        const device = deviceId ? selectableDevices(devices, kind).find((candidate) => candidate.deviceId === deviceId) : null
        if (deviceId && !device) throw new Error(`The selected ${KIND_LABELS[kind].singular} is no longer available.`)
        try {
          await applyDevice(kind, deviceId)
        } catch (error) {
          errors = { ...errors, [kind]: error?.message || `The ${KIND_LABELS[kind].singular} could not be changed.` }
          publish()
          throw error
        }
        effective = { ...effective, [kind]: deviceId }
        preferences = { ...preferences, [kind]: preferenceForDevice(device) }
        errors = { ...errors, [kind]: '' }
        details = { ...details, [kind]: device ? `Using ${device.label || `the selected ${KIND_LABELS[kind].singular}`}.` : `Using the system default ${KIND_LABELS[kind].singular}.` }
        persist()
        publish()
        return snapshot()
      })
    },
    whenIdle() { return queue },
    dispose() {
      disposed = true
      mediaDevices?.removeEventListener?.('devicechange', handleDeviceChange)
    },
  }
}
