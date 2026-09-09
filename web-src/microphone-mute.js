export function createMicrophoneMuteController(onChange = () => {}) {
  let muted = false

  const disableOutboundAudio = (session) => {
    for (const sender of session?.connection?.getSenders?.() || []) {
      if (sender.track?.kind === 'audio') sender.track.enabled = false
    }
  }

  const apply = (session) => {
    if (!session) return muted
    if (muted) {
      session.mute?.({ audio: true })
      // JsSIP remembers its muted flag, so mute() can be a no-op after a
      // renegotiation replaces the sender. Cover the replacement explicitly.
      disableOutboundAudio(session)
    } else {
      // Let JsSIP restore media so its hold state remains authoritative.
      session.unmute?.({ audio: true })
    }
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
