export function createMicrophoneInputController({
  mediaDevices = globalThis.navigator?.mediaDevices,
  getSession = () => null,
  isMuted = () => false,
  diagnose = () => {},
} = {}) {
  let deviceId = ''
  let selectedTrack = null
  let queue = Promise.resolve()

  const constraint = () => deviceId ? { deviceId: { exact: deviceId } } : true
  const release = () => {
    if (!selectedTrack) return
    try { selectedTrack.stop() } catch {}
    selectedTrack = null
  }
  const setDeviceNow = async (requestedDeviceId) => {
    const current = getSession()
    const sender = current?.connection?.getSenders?.().find((candidate) => candidate.track?.kind === 'audio')
    if (!sender) {
      deviceId = requestedDeviceId
      diagnose('microphone.preference-changed', { device_id: requestedDeviceId, applied_to_call: false }, current)
      return
    }
    if (!mediaDevices?.getUserMedia || typeof sender.replaceTrack !== 'function') {
      throw new Error('This browser cannot switch microphones during a call.')
    }
    const stream = await mediaDevices.getUserMedia({
      audio: requestedDeviceId ? { deviceId: { exact: requestedDeviceId } } : true,
      video: false,
    })
    const nextTrack = stream.getAudioTracks?.()[0]
    if (!nextTrack) {
      for (const track of stream.getTracks?.() || []) track.stop()
      throw new Error('The selected microphone did not provide an audio track.')
    }
    if (getSession() !== current) {
      nextTrack.stop()
      deviceId = requestedDeviceId
      diagnose('microphone.preference-changed', { device_id: requestedDeviceId, applied_to_call: false, reason: 'call-changed' }, current)
      return
    }
    const previousTrack = sender.track
    try {
      await sender.replaceTrack(nextTrack)
    } catch (error) {
      nextTrack.stop()
      diagnose('microphone.switch-failed', { device_id: requestedDeviceId, name: error?.name, message: error?.message }, current)
      throw error
    }
    if (getSession() !== current) {
      nextTrack.stop()
      deviceId = requestedDeviceId
      diagnose('microphone.preference-changed', { device_id: requestedDeviceId, applied_to_call: false, reason: 'call-ended-during-switch' }, current)
      return
    }
    deviceId = requestedDeviceId
    nextTrack.enabled = !isMuted()
    if (previousTrack && previousTrack !== nextTrack) previousTrack.stop()
    if (selectedTrack && selectedTrack !== previousTrack && selectedTrack !== nextTrack) selectedTrack.stop()
    selectedTrack = nextTrack
    diagnose('microphone.switched', { device_id: requestedDeviceId, track_id: nextTrack.id, label: nextTrack.label, muted: isMuted() }, current)
  }

  return {
    get deviceId() { return deviceId },
    constraint,
    setDevice(value = '') {
      const requestedDeviceId = String(value || '')
      const operation = queue.catch(() => {}).then(() => setDeviceNow(requestedDeviceId))
      queue = operation
      return operation
    },
    release,
    whenIdle() { return queue },
  }
}
