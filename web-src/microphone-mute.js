export function createMicrophoneMuteController(onChange = () => {}) {
  let muted = false

  const setTrackState = (session) => {
    for (const sender of session?.connection?.getSenders?.() || []) {
      if (sender.track?.kind === 'audio') sender.track.enabled = !muted
    }
  }

  const apply = (session) => {
    if (!session) return muted
    if (muted) session.mute?.({ audio: true })
    else session.unmute?.({ audio: true })
    setTrackState(session)
    return muted
  }

  const set = (value, session) => {
    const next = Boolean(value)
    const changed = next !== muted
    muted = next
    apply(session)
    if (changed) onChange(muted)
    return muted
  }

  return {
    get muted() { return muted },
    set,
    apply,
    reset(session) { return set(false, session) },
  }
}
