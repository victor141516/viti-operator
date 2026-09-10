export function audioLevelFromTimeDomain(samples) {
  if (!samples?.length) return 0
  let sum = 0
  for (const sample of samples) {
    const normalized = (sample - 128) / 128
    sum += normalized * normalized
  }
  const rms = Math.sqrt(sum / samples.length)
  if (rms <= 0.001) return 0
  const decibels = 20 * Math.log10(rms)
  return Math.max(0, Math.min(1, (decibels + 60) / 60))
}

export function createAudioLevelMeter({
  AudioContextClass = globalThis.AudioContext || globalThis.webkitAudioContext,
  setTimer = globalThis.setInterval,
  clearTimer = globalThis.clearInterval,
  onLevel = () => {},
  intervalMs = 100,
} = {}) {
  let context = null
  let source = null
  let analyser = null
  let timer = null
  let currentStream = null

  const stop = () => {
    if (timer !== null) clearTimer(timer)
    timer = null
    try { source?.disconnect?.() } catch {}
    source = null
    analyser = null
    currentStream = null
    const closing = context
    context = null
    closing?.close?.().catch?.(() => {})
    onLevel(0)
  }

  const attach = (stream) => {
    if (stream === currentStream) return true
    stop()
    if (!stream || !AudioContextClass) return false
    context = new AudioContextClass()
    analyser = context.createAnalyser()
    analyser.fftSize = 512
    analyser.smoothingTimeConstant = 0.72
    source = context.createMediaStreamSource(stream)
    // The media element/sender remains the only playback/transmit route. This
    // tap is intentionally not connected to context.destination, preventing
    // duplicate playback and acoustic echo.
    source.connect(analyser)
    currentStream = stream
    const samples = new Uint8Array(analyser.fftSize)
    const sample = () => {
      analyser.getByteTimeDomainData(samples)
      onLevel(audioLevelFromTimeDomain(samples))
    }
    timer = setTimer(sample, intervalMs)
    sample()
    context.resume?.().catch?.(() => {})
    return true
  }

  return { attach, stop, get stream() { return currentStream } }
}
