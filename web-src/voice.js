import JsSIP from 'jssip'

export function createVoice(onEvent, audioElement, identity = {}) {
  let ua = null
  let session = null
  let testAudio = null
  let ringback = null
  let incomingRing = null
  let incomingAudioContext = null
  let endTone = null
  let localHangup = false
  let iceServers = []
  const peerConfig = () => ({
    iceServers,
    // The embedded browser may suppress every host candidate.  When our
    // private TURN endpoint is provisioned, use it deterministically instead
    // of delaying the SIP INVITE while unreachable interfaces time out.
    iceTransportPolicy: iceServers.some((server) => {
      const urls = Array.isArray(server?.urls) ? server.urls : [server?.urls]
      return urls.some((url) => String(url || '').startsWith('turn:'))
    }) ? 'relay' : 'all',
  })
  const monitoredConnections = new WeakSet()
  const monitoredDTMFSenders = new WeakSet()
  const sessionTraceIDs = new WeakMap()
  let pendingTraceID = ''
  const clientId = identity.clientId || globalThis.crypto?.randomUUID?.() || `client-${Date.now()}-${Math.random()}`
  const clientName = identity.clientName || (/Android/i.test(navigator.userAgent) ? 'Android device' : 'Web browser')
  let diagnosticsEnabled = false
  const sessionId = (value) => value?.request?.call_id || value?._request?.call_id || ''
  const requestHeader = (value, name) => value?.request?.getHeader?.(name) || value?._request?.getHeader?.(name) || ''
  const traceId = (value = session) => sessionTraceIDs.get(value) || requestHeader(value, 'X-Viti-Trace-ID') || pendingTraceID || sessionId(value)
  const participantId = (value = session) => requestHeader(value, 'X-Viti-Participant-ID') || clientId
  const emit = (type, detail = {}) => onEvent(type, { participant_id: detail.participant_id || participantId(), client_id: clientId, ...detail, trace_id: detail.trace_id || traceId() })
  const nativeAndroidRinging = /Android/i.test(navigator.userAgent)
  const diagnose = (kind, data = {}, current = session) => {
    if (!diagnosticsEnabled) return
    emit('diagnostic', {
    at: new Date().toISOString(), at_ns: Math.round((performance.timeOrigin + performance.now()) * 1e6),
    source: /Android/i.test(navigator.userAgent) ? 'android-webview' : 'web-voice',
    client_id: clientId, session_id: sessionId(current), trace_id: traceId(current), kind, data,
    })
  }

  if (!globalThis.__vitiGlobalDiagnosticsBound) {
    globalThis.__vitiGlobalDiagnosticsBound = true
    globalThis.addEventListener('error', (event) => diagnose('runtime.error', {
      message: event.message, filename: event.filename, lineno: event.lineno, colno: event.colno,
      error: event.error ? { name: event.error.name, message: event.error.message, stack: event.error.stack } : null,
    }))
    globalThis.addEventListener('unhandledrejection', (event) => diagnose('runtime.unhandled-rejection', {
      reason: event.reason instanceof Error
        ? { name: event.reason.name, message: event.reason.message, stack: event.reason.stack }
        : String(event.reason),
    }))
  }

  const getIncomingAudioContext = () => {
    if (incomingAudioContext?.state !== 'closed') return incomingAudioContext
    const AudioContext = window.AudioContext || window.webkitAudioContext
    if (!AudioContext) return null
    incomingAudioContext = new AudioContext()
    return incomingAudioContext
  }
  const unlockIncomingAudio = () => {
    if (nativeAndroidRinging) return
    const context = getIncomingAudioContext()
    if (context?.state === 'suspended') context.resume().catch(() => {})
  }
  globalThis.addEventListener('pointerdown', unlockIncomingAudio, { once: true, capture: true })
  globalThis.addEventListener('keydown', unlockIncomingAudio, { once: true, capture: true })

  const stopRingback = (reason) => {
    if (!ringback) return
    try { clearInterval(ringback.timer) } catch {}
    try { ringback.oscillator.stop() } catch {}
    try { ringback.context.close() } catch {}
    ringback = null
    diagnose('ringback.stopped', { reason })
  }
  const startRingback = () => {
    if (ringback) return
    const AudioContext = window.AudioContext || window.webkitAudioContext
    if (!AudioContext) return
    const context = new AudioContext()
    const oscillator = context.createOscillator()
    const gain = context.createGain()
    oscillator.frequency.value = 425
    oscillator.type = 'sine'
    gain.gain.value = 0.055
    oscillator.connect(gain).connect(context.destination)
    oscillator.start()
    const silence = () => { if (ringback?.gain === gain) gain.gain.setValueAtTime(0, context.currentTime) }
    const pulse = () => {
      if (ringback?.gain !== gain) return
      gain.gain.setValueAtTime(0.055, context.currentTime)
      setTimeout(silence, 1000)
    }
    const timer = setInterval(pulse, 6000)
    ringback = { context, oscillator, gain, timer }
    setTimeout(silence, 1000)
    context.resume().catch(() => {})
    diagnose('ringback.started', { frequency_hz: 425, cadence_ms: [1000, 5000] })
  }

  const stopIncomingRing = (reason) => {
    if (!incomingRing) return
    try { clearInterval(incomingRing.timer) } catch {}
    try { clearTimeout(incomingRing.silenceTimer) } catch {}
    for (const oscillator of incomingRing.oscillators) {
      try { oscillator.stop() } catch {}
    }
    incomingRing = null
    diagnose('incoming-ring.stopped', { reason })
  }
  const startIncomingRing = () => {
    if (nativeAndroidRinging) {
      diagnose('incoming-ring.delegated-to-native')
      return
    }
    if (incomingRing) return
    const context = getIncomingAudioContext()
    if (!context) {
      diagnose('incoming-ring.unavailable')
      return
    }
    const gain = context.createGain()
    const oscillators = [440, 480].map((frequency) => {
      const oscillator = context.createOscillator()
      oscillator.frequency.value = frequency
      oscillator.type = 'sine'
      oscillator.connect(gain)
      oscillator.start()
      return oscillator
    })
    gain.connect(context.destination)
    gain.gain.value = 0
    const silence = () => {
      if (incomingRing?.gain === gain) gain.gain.setValueAtTime(0, context.currentTime)
    }
    const pulse = () => {
      if (incomingRing?.gain !== gain) return
      gain.gain.setValueAtTime(0.055, context.currentTime)
      incomingRing.silenceTimer = setTimeout(silence, 1200)
    }
    const timer = setInterval(pulse, 4000)
    incomingRing = { context, gain, oscillators, timer, silenceTimer: null }
    pulse()
    context.resume()
      .then(() => diagnose('incoming-ring.started', { frequencies_hz: [440, 480], cadence_ms: [1200, 2800] }))
      .catch((error) => {
        diagnose('incoming-ring.resume-failed', { name: error?.name, message: error?.message })
        emit('audio-blocked')
      })
  }

  const playEndTone = (reason) => {
    if (endTone) return endTone
    const AudioContext = window.AudioContext || window.webkitAudioContext
    if (!AudioContext) {
      diagnose('end-tone.unavailable', { reason })
      return Promise.resolve()
    }
    const context = new AudioContext()
    const oscillator = context.createOscillator()
    const gain = context.createGain()
    const durationMs = 250
    const now = context.currentTime
    oscillator.frequency.value = 425
    oscillator.type = 'sine'
    gain.gain.setValueAtTime(0, now)
    gain.gain.linearRampToValueAtTime(0.065, now + 0.008)
    gain.gain.setValueAtTime(0.065, now + 0.22)
    gain.gain.linearRampToValueAtTime(0, now + durationMs / 1000)
    oscillator.connect(gain).connect(context.destination)
    oscillator.start(now)
    oscillator.stop(now + durationMs / 1000)
    diagnose('end-tone.started', { reason, frequency_hz: 425, duration_ms: durationMs })
    const current = context.resume()
      .catch((error) => diagnose('end-tone.resume-failed', { reason, name: error?.name, message: error?.message }))
      .then(() => new Promise((resolve) => {
        setTimeout(() => {
          context.close().catch(() => {})
          diagnose('end-tone.finished', { reason, duration_ms: durationMs })
          resolve()
        }, durationMs)
      }))
    const tracked = current.finally(() => { if (endTone === tracked) endTone = null })
    endTone = tracked
    return endTone
  }

  const attachAudio = (stream, current = session) => {
    if (!stream || !audioElement) return
    stopRingback('remote-track')
    audioElement.srcObject = stream
    diagnose('audio.attached', {
      stream_id: stream.id,
      tracks: stream.getTracks().map((track) => ({ id: track.id, kind: track.kind, label: track.label, enabled: track.enabled, muted: track.muted, ready_state: track.readyState })),
    }, current)
    audioElement.play().then(() => diagnose('audio.playing', {}, current)).catch((error) => {
      diagnose('audio.play.failed', { name: error?.name, message: error?.message }, current)
      emit('audio-blocked')
    })
  }

  if (audioElement && !audioElement.__vitiDiagnosticBound) {
    audioElement.__vitiDiagnosticBound = true
    for (const name of ['abort', 'canplay', 'emptied', 'ended', 'error', 'loadeddata', 'pause', 'play', 'playing', 'stalled', 'suspend', 'waiting']) {
      audioElement.addEventListener(name, () => diagnose(`audio.element.${name}`, {
        current_time: audioElement.currentTime, paused: audioElement.paused,
        ready_state: audioElement.readyState, network_state: audioElement.networkState,
        error: audioElement.error ? { code: audioElement.error.code, message: audioElement.error.message } : null,
      }))
    }
  }

  const bindSession = (next) => {
    if (next.__vitiBound) return
    next.__vitiBound = true
    const currentTraceID = traceId(next) || globalThis.crypto?.randomUUID?.() || `trace-${Date.now()}-${Math.random()}`
    sessionTraceIDs.set(next, currentTraceID)
    pendingTraceID = ''
    session = next
    diagnose('session.created', {
      direction: next.direction, local_identity: next.local_identity?.toString?.(),
      remote_identity: next.remote_identity?.toString?.(), request: next.request?.toString?.(),
    }, next)
    if (next.direction === 'incoming') {
      const from = next.remote_identity?.uri?.user || 'Private number'
      startIncomingRing()
      emit('incoming', { from })
    }
    for (const name of ['connecting', 'sending', 'peerconnection', 'sdp', 'icecandidate', 'getusermediafailed', 'hold', 'unhold', 'muted', 'unmuted', 'newDTMF', 'newInfo', 'reinvite', 'update', 'refer', 'replaces']) {
      next.on(name, (event = {}) => diagnose(`session.${name.toLowerCase()}`, {
        originator: event.originator, cause: event.cause,
        request: event.request?.toString?.(), response: event.response?.toString?.(),
        sdp: event.sdp, type: event.type, candidate: event.candidate?.toJSON?.() || event.candidate,
        error: event.error ? { name: event.error.name, message: event.error.message, stack: event.error.stack } : null,
      }, next))
    }
    next.on('icecandidate', ({ candidate, ready } = {}) => {
      if (candidate?.candidate?.includes(' typ relay ') && typeof ready === 'function') {
        diagnose('ice.relay-ready', { candidate: candidate.toJSON?.() || candidate }, next)
        // JsSIP exposes ready() so the application can stop gathering once it
        // has a usable route. Some Windows interfaces otherwise keep the SIP
        // INVITE waiting even after TURN has supplied that route.
        ready()
      }
    })
    next.on('peerconnection:setremotedescriptionfailed', (error) => {
      diagnose('peer.set-remote-description.failed', {
        error: error ? { name: error.name, message: error.message, stack: error.stack } : null,
        signaling_state: next.connection?.signalingState,
        local_description: next.connection?.localDescription,
        remote_description: next.connection?.remoteDescription,
      }, next)
    })
    next.on('progress', (event) => {
      const code = event?.response?.status_code || 0
      const reason = event?.response?.reason_phrase || ''
      diagnose('session.progress', { code, reason, response: event?.response?.toString?.() }, next)
      if (next.direction !== 'outgoing') return
      emit('ringing', { code, reason })
      if (code === 183) stopRingback('early-media')
      else if (code === 180) startRingback()
    })
    const monitorConnection = (peerconnection) => {
      if (!peerconnection || monitoredConnections.has(peerconnection)) return
      monitoredConnections.add(peerconnection)
      const snapshot = (kind) => diagnose(kind, {
        connection_state: peerconnection.connectionState,
        ice_connection_state: peerconnection.iceConnectionState,
        ice_gathering_state: peerconnection.iceGatheringState,
        signaling_state: peerconnection.signalingState,
        local_description: peerconnection.localDescription ? { type: peerconnection.localDescription.type, sdp: peerconnection.localDescription.sdp } : null,
        remote_description: peerconnection.remoteDescription ? { type: peerconnection.remoteDescription.type, sdp: peerconnection.remoteDescription.sdp } : null,
      }, next)
      for (const [property, eventName] of [
        ['onconnectionstatechange', 'peer.connection-state'], ['oniceconnectionstatechange', 'peer.ice-connection-state'],
        ['onicegatheringstatechange', 'peer.ice-gathering-state'], ['onsignalingstatechange', 'peer.signaling-state'],
        ['onnegotiationneeded', 'peer.negotiation-needed'],
      ]) {
        const previous = peerconnection[property]
        peerconnection[property] = (event) => { previous?.call(peerconnection, event); snapshot(eventName) }
      }
      const previousTrack = peerconnection.ontrack
      peerconnection.ontrack = (event) => {
        previousTrack?.call(peerconnection, event)
        const { streams, track, receiver, transceiver } = event
        diagnose('peer.track', {
          track: { id: track.id, kind: track.kind, label: track.label, enabled: track.enabled, muted: track.muted, ready_state: track.readyState },
          streams: (streams || []).map((item) => item.id), receiver_parameters: receiver?.getParameters?.(), transceiver_mid: transceiver?.mid,
        }, next)
        for (const name of ['mute', 'unmute', 'ended']) track.addEventListener(name, () => diagnose(`track.${name}`, {
          id: track.id, enabled: track.enabled, muted: track.muted, ready_state: track.readyState,
        }, next))
        attachAudio(streams?.[0] || new MediaStream([track]), next)
      }
      const mediaTimer = setInterval(async () => {
        if (session !== next) { clearInterval(mediaTimer); return }
        try {
          const stats = await peerconnection.getStats()
          const reports = []
          let sent = 0, received = 0
          stats.forEach((report) => {
            reports.push({ ...report })
            if (report.type === 'outbound-rtp' && report.kind === 'audio') sent += report.bytesSent || 0
            if (report.type === 'inbound-rtp' && report.kind === 'audio') received += report.bytesReceived || 0
          })
          diagnose('peer.stats', { reports }, next)
          emit('media', { sent: sent > 0, received: received > 0 })
        } catch (error) { diagnose('peer.stats.failed', { name: error?.name, message: error?.message }, next) }
      }, 1000)
      snapshot('peer.monitoring-started')
    }
    next.on('accepted', (event) => { next.__vitiWasActive = true; stopRingback('accepted'); stopIncomingRing('accepted'); diagnose('session.accepted', { response: event?.response?.toString?.() }, next); emit('active'); monitorConnection(next.connection) })
    next.on('confirmed', (event) => { next.__vitiWasActive = true; stopRingback('confirmed'); stopIncomingRing('confirmed'); diagnose('session.confirmed', { ack: event?.ack?.toString?.() }, next); emit('active'); monitorConnection(next.connection) })
    const stopTestAudio = () => {
      if (!testAudio) return
      try { testAudio.oscillator.stop() } catch {}
      try { testAudio.context.close() } catch {}
      testAudio = null
    }
    next.on('ended', (event) => {
      stopRingback('ended'); stopIncomingRing('ended'); stopTestAudio(); diagnose('session.ended', { cause: event?.cause, message: event?.message?.toString?.() }, next)
      if (session === next) session = null
      localHangup = false
      const finish = () => { if (!next.__vitiLocallyEnded) emit('ended', { trace_id: traceId(next) }) }
      if (next.__vitiWasActive) playEndTone('remote-ended').finally(finish)
      else finish()
    })
    next.on('failed', (event) => {
      stopRingback('failed'); stopIncomingRing('failed'); stopTestAudio(); diagnose('session.failed', { cause: event?.cause, message: event?.message?.toString?.() }, next)
      if (session === next) session = null
      const endedLocally = localHangup
      localHangup = false
      const finish = () => { if (!next.__vitiLocallyEnded) emit(endedLocally ? 'ended' : 'failed', { trace_id: traceId(next), cause: event?.cause || '' }) }
      if (next.__vitiWasActive) playEndTone('active-call-failed').finally(finish)
      else finish()
    })
    next.on('peerconnection', ({ peerconnection }) => monitorConnection(peerconnection))
    monitorConnection(next.connection)
  }

  return {
    start(provisioning) {
      if (ua) ua.stop()
      iceServers = provisioning.ice_servers || []
      diagnosticsEnabled = provisioning.diagnostics_enabled === true
      diagnose('ua.starting', { provisioning })
      const socket = new JsSIP.WebSocketInterface(provisioning.websocket_url)
      ua = new JsSIP.UA({ sockets: [socket], uri: `sip:${provisioning.username}@${provisioning.domain}`, password: provisioning.password, display_name: clientName, user_agent: `Viti Operator · ${clientName}`, register: true, session_timers: false })
      ua.on('connecting', (event) => diagnose('ua.connecting', { attempts: event?.attempts }))
      ua.on('connected', () => { diagnose('ua.connected'); emit('connected') })
      ua.on('registered', (event) => { diagnose('ua.registered', { response: event?.response?.toString?.() }); emit('registered') })
      ua.on('unregistered', (event) => diagnose('ua.unregistered', { cause: event?.cause }))
      ua.on('disconnected', (event) => { diagnose('ua.disconnected', { code: event?.code, reason: event?.reason }); emit('disconnected') })
      ua.on('registrationFailed', (event) => { diagnose('ua.registration-failed', { cause: event?.cause, response: event?.response?.toString?.() }); emit('registration-failed', { cause: event?.cause || '' }) })
      ua.on('newRTCSession', ({ session: next, request }) => { diagnose('ua.new-session', { request: request?.toString?.() }, next); bindSession(next) })
      ua.start()
    },
    call(number, suppliedTraceID = '') {
      if (!ua || !ua.isRegistered()) throw new Error('The WebRTC phone is not registered yet')
      stopIncomingRing('outgoing-call')
      pendingTraceID = suppliedTraceID || globalThis.crypto?.randomUUID?.() || `trace-${Date.now()}-${Math.random()}`
      const options = {
        mediaConstraints: { audio: true, video: false }, pcConfig: peerConfig(),
        extraHeaders: [`X-Viti-Trace-ID: ${pendingTraceID}`, `X-Viti-Client-ID: ${clientId}`],
      }
      if (new URLSearchParams(location.search).get('testTone') === '1') {
        const AudioContext = window.AudioContext || window.webkitAudioContext
        const context = new AudioContext(), oscillator = context.createOscillator(), gain = context.createGain(), destination = context.createMediaStreamDestination()
        oscillator.frequency.value = 697; gain.gain.value = 0.025; oscillator.connect(gain).connect(destination); oscillator.start()
        testAudio = { context, oscillator }; options.mediaStream = destination.stream
      }
      diagnose('action.call', { number, trace_id: pendingTraceID })
      session = ua.call(`sip:${number}@${ua.configuration.uri.host}`, options)
      bindSession(session)
      emit('calling', { to: number, trace_id: traceId(session) })
    },
    answer() { if (session) { stopIncomingRing('answer'); diagnose('action.answer'); session.answer({ mediaConstraints: { audio: true, video: false }, pcConfig: peerConfig(), extraHeaders: [`X-Viti-Client-ID: ${clientId}`] }) } },
    reject() { if (session) { stopIncomingRing('reject'); diagnose('action.reject'); session.terminate({ status_code: 603, reason_phrase: 'Decline' }) } },
    hangup() {
      if (!session) return
      const ending = session
      diagnose('action.hangup', {}, ending)
      ending.__vitiLocallyEnded = true
      localHangup = false
      stopRingback('local-hangup')
      stopIncomingRing('local-hangup')
      if (session === ending) session = null
      const finish = () => {
        try { ending.terminate() } finally { emit('ended', { trace_id: traceId(ending) }) }
      }
      if (ending.__vitiWasActive) playEndTone('local-hangup').finally(finish)
      else finish()
    },
    mute(value) { if (session) { diagnose('action.mute', { value }); value ? session.mute({ audio: true }) : session.unmute({ audio: true }) } },
    dtmf(tone) {
      if (!session || !session.isEstablished()) throw new Error('The call is not connected yet')
      if (!/^[0-9*#]$/.test(String(tone))) throw new Error('Invalid DTMF key')
      const audioSender = session.connection?.getSenders?.().find((sender) => sender.track?.kind === 'audio')
      const dtmfSender = audioSender?.dtmf
      if (!dtmfSender?.canInsertDTMF) throw new Error('This browser cannot send DTMF tones in this call')
      if (!monitoredDTMFSenders.has(dtmfSender)) {
        monitoredDTMFSenders.add(dtmfSender)
        dtmfSender.addEventListener?.('tonechange', (event) => diagnose('dtmf.tone-change', { tone: event.tone }))
      }
      diagnose('action.dtmf', { tone, transport: 'RFC4733', duration_ms: 160, inter_tone_gap_ms: 100 })
      session.sendDTMF(String(tone), { transportType: 'RFC2833', duration: 160, interToneGap: 100 })
      emit('dtmf', { tone: String(tone) })
    },
    stop() { diagnose('ua.stop'); stopRingback('ua-stop'); stopIncomingRing('ua-stop'); if (session) session.terminate(); if (ua) ua.stop(); if (incomingAudioContext) incomingAudioContext.close().catch(() => {}); incomingAudioContext = null; session = null; ua = null },
  }
}
