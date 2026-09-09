export function recordingTrackPresence(track, positionMs) {
  const offsetMs = Math.max(0, Number(track?.offset_ms) || 0)
  const durationValue = Number(track?.duration_ms)
  const durationMs = Number.isFinite(durationValue) && durationValue > 0 ? durationValue : null
  const currentMs = Math.max(0, Number(positionMs) || 0)
  const endMs = durationMs === null ? null : offsetMs + durationMs

  if (currentMs < offsetMs) return { active: false, phase: 'before', offsetMs, endMs }
  if (endMs !== null && currentMs >= endMs) return { active: false, phase: 'after', offsetMs, endMs }
  return { active: true, phase: 'active', offsetMs, endMs }
}
