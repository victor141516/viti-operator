import JsSIP from 'jssip'
import { createMicrophoneMuteController } from './microphone-mute.js'
import { createMicrophoneInputController } from './microphone-input.js'
import { createCallQualitySampler } from './call-quality.js'
import { createAudioLevelMeter } from './audio-level-meter.js'

export function createVoice(onEvent, audioElement, identity = {}) {
  let ua = null
  let session = null
  const sessionsByLegToken = new Map()
  let testAudio = null
  let ringback = null
  let incomingRing = null
  let incomingAudioContext = null
  let endTone = null
  let dtmfFeedback = null
  let pendingLegToken = ''
	let pendingConnectionID = ''
  let telecomMuted = false
  let systemInactive = false
  let lastTerminationClassification = null
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
  const legToken = (value = session) => value?.__vitiLegToken || ''
	const connectionId = (value = session) => value?.__vitiConnectionID || requestHeader(value, 'X-Viti-Connection-ID') || ''
  const traceId = (value = session) => sessionTraceIDs.get(value) || requestHeader(value, 'X-Viti-Trace-ID') || pendingTraceID || sessionId(value)
  const participantId = (value = session) => requestHeader(value, 'X-Viti-Participant-ID') || clientId
  const emit = (type, detail = {}, current = session) => onEvent(type, {
    participant_id: detail.participant_id || participantId(current), client_id: clientId,
	...detail, trace_id: detail.trace_id || traceId(current), leg_token: detail.leg_token || legToken(current), connection_id: detail.connection_id || connectionId(current),
  })
  const emitQuality = (sample, current) => {
    current.__vitiQualitySequence = (current.__vitiQualitySequence || 0) + 1
    emit('quality', { quality: { ...sample, sequence: current.__vitiQualitySequence } }, current)
  }
  const stopQuality = (current, status = '') => {
    if (!current) return
    current.__vitiQualitySampler?.stop()
    current.__vitiQualitySampler = null
    current.__vitiQualityPeer = null
    if (status) emitQuality({
      status, sampledAtMs: Date.now(), inboundLossPercent: null, outboundLossPercent: null,
      inboundJitterMs: null, roundTripTimeMs: null, roundTripScope: '', inboundWindowMs: null, outboundWindowMs: null, route: null,
    }, current)
  }
  const microphoneMute = createMicrophoneMuteController((muted) => {
    diagnose('microphone.mute-changed', { muted })
    emit('mute-changed', { muted })
  })
  const nativeAndroidRinging = /Android/i.test(navigator.userAgent)
  const diagnose = (kind, data = {}, current = session) => {
    if (!diagnosticsEnabled) return
    emit('diagnostic', {
    at: new Date().toISOString(), at_ns: Math.round((performance.timeOrigin + performance.now()) * 1e6),
    source: /Android/i.test(navigator.userAgent) ? 'android-webview' : 'web-voice',
    client_id: clientId, session_id: sessionId(current), trace_id: traceId(current), leg_token: legToken(current), kind, data,
    }, current)
  }
  let localLevelTrack = null
  const microphoneLevel = createAudioLevelMeter({ onLevel: (level) => emit('audio-level', { kind: 'microphone', level }) })
  const speakerLevel = createAudioLevelMeter({ onLevel: (level) => emit('audio-level', { kind: 'speaker', level }) })
  const attachMicrophoneLevel = (track, current = session) => {
    if (track === localLevelTrack) return
    localLevelTrack = track || null
    if (!track || track.readyState === 'ended') {
      microphoneLevel.stop()
      return
    }
    microphoneLevel.attach(new MediaStream([track]))
    diagnose('audio-level.microphone-attached', { track_id: track.id, enabled: track.enabled, muted: track.muted }, current)
  }
  const stopLevelMeters = () => {
    localLevelTrack = null
    microphoneLevel.stop()
    speakerLevel.stop()
  }
  const cancelDTMFFeedback = () => {
    if (!dtmfFeedback) return
    for (const oscillator of dtmfFeedback.oscillators) { try { oscillator.stop() } catch {} }
    dtmfFeedback.context?.close?.().catch?.(() => {})
    dtmfFeedback = null
  }
  const microphoneInput = createMicrophoneInputController({
    mediaDevices: navigator.mediaDevices,
    getSession: () => session,
    isMuted: () => microphoneMute.muted,
    diagnose,
    onTrackChanged: attachMicrophoneLevel,
  })
  const applySystemAudioState = (current = session) => {
    microphoneMute.setSuppressed(telecomMuted || systemInactive, current)
    if (audioElement) audioElement.muted = systemInactive
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
    speakerLevel.attach(stream)
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
    if (next.__vitiBound) return legToken(next)
    next.__vitiBound = true
    next.__vitiLegToken = next.__vitiLegToken || (
      next.direction === 'outgoing' ? pendingLegToken : ''
    ) || globalThis.crypto?.randomUUID?.() || `leg-${Date.now()}-${Math.random()}`
	next.__vitiConnectionID = next.__vitiConnectionID || (
	  next.direction === 'outgoing' ? pendingConnectionID : requestHeader(next, 'X-Viti-Connection-ID')
	) || globalThis.crypto?.randomUUID?.() || `connection-${Date.now()}-${Math.random()}`
    pendingLegToken = ''
	pendingConnectionID = ''
    sessionsByLegToken.set(next.__vitiLegToken, next)
    const currentTraceID = traceId(next) || globalThis.crypto?.randomUUID?.() || `trace-${Date.now()}-${Math.random()}`
    sessionTraceIDs.set(next, currentTraceID)
    pendingTraceID = ''
    if (!session) session = next
    diagnose('session.created', {
      direction: next.direction, local_identity: next.local_identity?.toString?.(),
      remote_identity: next.remote_identity?.toString?.(), request: next.request?.toString?.(),
    }, next)
    if (next.direction === 'incoming') {
      const from = next.remote_identity?.uri?.user || 'Private number'
      startIncomingRing()
      emit('incoming', { from }, next)
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
    next.on('peerconnection', () => microphoneMute.apply(next))
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
      emit('ringing', { code, reason }, next)
      if (code === 183) stopRingback('early-media')
      else if (code === 180) startRingback()
    })
    const monitorConnection = (peerconnection) => {
      if (!peerconnection || monitoredConnections.has(peerconnection)) return
      monitoredConnections.add(peerconnection)
      attachMicrophoneLevel(peerconnection.getSenders?.().find((sender) => sender.track?.kind === 'audio')?.track, next)
      if (next.__vitiQualityPeer && next.__vitiQualityPeer !== peerconnection) stopQuality(next, 'connecting')
      next.__vitiQualityPeer = peerconnection
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
        peerconnection[property] = (event) => { previous?.call(peerconnection, event); attachMicrophoneLevel(peerconnection.getSenders?.().find((sender) => sender.track?.kind === 'audio')?.track, next); snapshot(eventName) }
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
      const qualitySampler = createCallQualitySampler({
        peerConnection: peerconnection,
        onSample: (sample) => emitQuality(sample, next),
        onError: (error) => diagnose('peer.stats.failed', { name: error?.name, message: error?.message }, next),
        onStats: (stats) => {
          const reports = []
          let sent = 0, received = 0
          stats.forEach((report) => {
            reports.push({ ...report })
            if (report.type === 'outbound-rtp' && report.kind === 'audio') sent += report.bytesSent || 0
            if (report.type === 'inbound-rtp' && report.kind === 'audio') received += report.bytesReceived || 0
          })
          diagnose('peer.stats', { reports }, next)
          emit('media', { sent: sent > 0, received: received > 0 }, next)
        },
      })
      next.__vitiQualitySampler = qualitySampler
      qualitySampler.start()
      snapshot('peer.monitoring-started')
      applySystemAudioState(next)
    }
    next.on('accepted', (event) => { next.__vitiWasActive = true; stopRingback('accepted'); stopIncomingRing('accepted'); diagnose('session.accepted', { response: event?.response?.toString?.() }, next); applySystemAudioState(next); emit('active', {}, next); monitorConnection(next.connection) })
    next.on('confirmed', (event) => { next.__vitiWasActive = true; stopRingback('confirmed'); stopIncomingRing('confirmed'); diagnose('session.confirmed', { ack: event?.ack?.toString?.() }, next); applySystemAudioState(next); emit('active', {}, next); monitorConnection(next.connection) })
    const stopTestAudio = () => {
      if (!testAudio) return
      try { testAudio.oscillator.stop() } catch {}
      try { testAudio.context.close() } catch {}
      testAudio = null
    }
    next.on('ended', (event) => {
      stopRingback('ended'); stopIncomingRing('ended'); stopTestAudio(); cancelDTMFFeedback(); stopQuality(next, 'unavailable'); diagnose('session.ended', { cause: event?.cause, message: event?.message?.toString?.() }, next)
      const wasCurrent = session === next
      lastTerminationClassification = {
        event: 'ended', was_current: wasCurrent, was_active: next.__vitiWasActive === true,
        recovery: next.__vitiRecovery === true, locally_ended: next.__vitiLocallyEnded === true,
        trace_id: traceId(next), leg_token: legToken(next), connection_id: connectionId(next),
      }
      diagnose('session.termination-classified', lastTerminationClassification, next)
      if (wasCurrent) session = null
      sessionsByLegToken.delete(legToken(next))
      // A replacement connection deliberately supersedes and hangs up the old
      // WebRTC leg. Its late terminal event belongs to the old generation and
      // must not end (or play an end tone over) the replacement call.
      if (!wasCurrent) return
      if (wasCurrent) { microphoneInput.release(); stopLevelMeters() }
	  const interrupted = wasCurrent && (next.__vitiWasActive || next.__vitiRecovery) && !next.__vitiLocallyEnded
	  if (wasCurrent && !interrupted) { microphoneMute.reset(); telecomMuted = false; systemInactive = false; if (audioElement) audioElement.muted = false }
	  if (interrupted) { emit('interrupted', { trace_id: traceId(next), cause: event?.cause || 'Call media ended unexpectedly' }, next); return }
      const finish = () => { if (!next.__vitiLocallyEnded) emit('ended', { trace_id: traceId(next) }, next) }
      if (next.__vitiWasActive) playEndTone('remote-ended').finally(finish)
      else finish()
    })
    next.on('failed', (event) => {
      stopRingback('failed'); stopIncomingRing('failed'); stopTestAudio(); cancelDTMFFeedback(); stopQuality(next, 'unavailable'); diagnose('session.failed', { cause: event?.cause, message: event?.message?.toString?.() }, next)
      const wasCurrent = session === next
      lastTerminationClassification = {
        event: 'failed', was_current: wasCurrent, was_active: next.__vitiWasActive === true,
        recovery: next.__vitiRecovery === true, locally_ended: next.__vitiLocallyEnded === true,
        trace_id: traceId(next), leg_token: legToken(next), connection_id: connectionId(next),
      }
      diagnose('session.termination-classified', lastTerminationClassification, next)
      if (wasCurrent) session = null
      sessionsByLegToken.delete(legToken(next))
      if (!wasCurrent) return
      if (wasCurrent) { microphoneInput.release(); stopLevelMeters() }
	  const interrupted = wasCurrent && (next.__vitiWasActive || next.__vitiRecovery) && !next.__vitiLocallyEnded
	  if (wasCurrent && !interrupted) { microphoneMute.reset(); telecomMuted = false; systemInactive = false; if (audioElement) audioElement.muted = false }
	  if (interrupted) { emit('interrupted', { trace_id: traceId(next), cause: event?.cause || 'Call media failed unexpectedly' }, next); return }
      const finish = () => { if (!next.__vitiLocallyEnded) emit('failed', { trace_id: traceId(next), cause: event?.cause || '' }, next) }
      if (next.__vitiWasActive) playEndTone('active-call-failed').finally(finish)
      else finish()
    })
    next.on('peerconnection', ({ peerconnection }) => monitorConnection(peerconnection))
    monitorConnection(next.connection)
    return legToken(next)
  }

  const sessionFor = (requestedLegToken = '') => {
    const target = requestedLegToken ? sessionsByLegToken.get(requestedLegToken) : session
    if (!target) throw new Error(requestedLegToken ? 'This call leg is no longer available' : 'There is no local call')
    return target
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
	call(number, suppliedTraceID = '', suppliedLegToken = '', preserveState = false) {
      if (!ua || !ua.isRegistered()) throw new Error('The WebRTC phone is not registered yet')
      if (session) throw new Error('This client already has a call')
	  if (!preserveState) {
		microphoneMute.reset()
		telecomMuted = false
		systemInactive = false
		if (audioElement) audioElement.muted = false
	  }
      stopIncomingRing('outgoing-call')
      pendingTraceID = suppliedTraceID || globalThis.crypto?.randomUUID?.() || `trace-${Date.now()}-${Math.random()}`
      pendingLegToken = suppliedLegToken || globalThis.crypto?.randomUUID?.() || `leg-${Date.now()}-${Math.random()}`
	  pendingConnectionID = globalThis.crypto?.randomUUID?.() || `connection-${Date.now()}-${Math.random()}`
      const requestedLegToken = pendingLegToken
      const options = {
        mediaConstraints: { audio: microphoneInput.constraint(), video: false }, pcConfig: peerConfig(),
		extraHeaders: [`X-Viti-Trace-ID: ${pendingTraceID}`, `X-Viti-Client-ID: ${clientId}`, `X-Viti-Connection-ID: ${pendingConnectionID}`],
      }
      // Synthetic media is an end-to-end diagnostic hook, never a normal UI
      // mode. The test runner must opt this tab in through sessionStorage, so
      // an old test URL cannot silently replace a real microphone.
      if (sessionStorage.getItem('viti-e2e-test-tone') === '1' && new URLSearchParams(location.search).get('testTone') === '1') {
        const AudioContext = window.AudioContext || window.webkitAudioContext
        const context = new AudioContext(), oscillator = context.createOscillator(), gain = context.createGain(), destination = context.createMediaStreamDestination()
        oscillator.frequency.value = 697; gain.gain.value = 0.025; oscillator.connect(gain).connect(destination); oscillator.start()
        testAudio = { context, oscillator }; options.mediaStream = destination.stream
      }
      diagnose('action.call', { number, trace_id: pendingTraceID })
      const outgoing = ua.call(`sip:${number}@${ua.configuration.uri.host}`, options)
      outgoing.__vitiLegToken = outgoing.__vitiLegToken || requestedLegToken
	  outgoing.__vitiRecovery = preserveState
      session = outgoing
      bindSession(outgoing)
      emit('calling', { to: number, trace_id: traceId(outgoing) }, outgoing)
      return legToken(outgoing)
    },
	reconnect(suppliedTraceID, suppliedLegToken) {
	  return this.call('*44', suppliedTraceID, suppliedLegToken, true)
	},
	interruptForTest() {
	  if (new URLSearchParams(location.search).get('testDisconnect') !== '1') {
		throw new Error('The media interruption hook is available only in test mode')
	  }
	  if (!session) throw new Error('There is no local call to interrupt')
	  session.terminate()
	  return true
	},
	terminationForTest() {
	  if (new URLSearchParams(location.search).get('testDisconnect') !== '1') {
		throw new Error('Termination diagnostics are available only in test mode')
	  }
	  return lastTerminationClassification
	},
	callContext(requestedLegToken = '') {
	  const target = sessionFor(requestedLegToken)
	  return { trace_id: traceId(target), leg_token: legToken(target), connection_id: connectionId(target), muted: microphoneMute.muted }
	},
	abandon() {
	  microphoneInput.release()
	  stopLevelMeters()
	  microphoneMute.reset()
	  telecomMuted = false
	  systemInactive = false
	  if (audioElement) audioElement.muted = false
	},
    answer(requestedLegToken = '') { const target = sessionFor(requestedLegToken); stopIncomingRing('answer'); diagnose('action.answer', {}, target); target.answer({ mediaConstraints: { audio: microphoneInput.constraint(), video: false }, pcConfig: peerConfig(), extraHeaders: [`X-Viti-Client-ID: ${clientId}`] }); return true },
    reject(requestedLegToken = '') {
      const target = sessionFor(requestedLegToken)
      stopIncomingRing('reject')
      diagnose('action.reject', {}, target)
      target.__vitiLocallyEnded = true
      try {
        target.terminate({ status_code: 603, reason_phrase: 'Decline' })
      } catch (error) {
        target.__vitiLocallyEnded = false
        throw error
      }
      sessionsByLegToken.delete(legToken(target))
      stopQuality(target)
      if (session === target) session = null
      emit('ended', { trace_id: traceId(target) }, target)
      return true
    },
    hangup(requestedLegToken = '') {
      const ending = sessionFor(requestedLegToken)
      diagnose('action.hangup', {}, ending)
      ending.__vitiLocallyEnded = true
      cancelDTMFFeedback()
      stopRingback('local-hangup')
      stopIncomingRing('local-hangup')
      if (session === ending) { microphoneInput.release(); stopLevelMeters() }
      if (session === ending) session = null
      sessionsByLegToken.delete(legToken(ending))
      stopQuality(ending)
      if (!session) { microphoneMute.reset(); telecomMuted = false; systemInactive = false; if (audioElement) audioElement.muted = false }
      const finish = () => {
        try { ending.terminate() } finally { emit('ended', { trace_id: traceId(ending) }, ending) }
      }
      if (ending.__vitiWasActive) playEndTone('local-hangup').finally(finish)
      else finish()
      return true
    },
    mute(value, requestedLegToken = '') {
      const target = sessionFor(requestedLegToken)
      diagnose('action.mute', { value: Boolean(value) })
      return microphoneMute.set(value, target)
    },
    setTelecomMuted(value, requestedLegToken = '') {
      const target = sessionFor(requestedLegToken)
      telecomMuted = Boolean(value)
      applySystemAudioState(target)
      diagnose('action.telecom-muted', { value: telecomMuted }, target)
      return true
    },
    setSystemInactive(value, requestedLegToken = '') {
      const target = sessionFor(requestedLegToken)
      systemInactive = Boolean(value)
      applySystemAudioState(target)
      diagnose('action.system-inactive', { value: systemInactive }, target)
      return true
    },
    setAudioInput: (deviceId) => microphoneInput.setDevice(deviceId),
    dtmf(tone, requestedLegToken = '', duration = 160) {
      const target = sessionFor(requestedLegToken)
      if (!target.isEstablished()) throw new Error('The call is not connected yet')
      if (!/^[0-9*#]$/.test(String(tone))) throw new Error('Invalid DTMF key')
      const audioSender = target.connection?.getSenders?.().find((sender) => sender.track?.kind === 'audio')
      const dtmfSender = audioSender?.dtmf
      if (!dtmfSender?.canInsertDTMF) throw new Error('This browser cannot send DTMF tones in this call')
      if (!monitoredDTMFSenders.has(dtmfSender)) {
        monitoredDTMFSenders.add(dtmfSender)
        dtmfSender.addEventListener?.('tonechange', (event) => diagnose('dtmf.tone-change', { tone: event.tone }))
      }
      const boundedDuration = Math.max(70, Math.min(6000, Math.round(Number(duration) || 160)))
      diagnose('action.dtmf', { tone, transport: 'RFC4733', duration_ms: boundedDuration, inter_tone_gap_ms: 100 })
      target.sendDTMF(String(tone), { transportType: 'RFC2833', duration: boundedDuration, interToneGap: 100 })
      emit('dtmf', { tone: String(tone), duration_ms: boundedDuration }, target)
    },
    startDTMF(tone, requestedLegToken = '') {
      const target = sessionFor(requestedLegToken)
      if (!target.isEstablished()) throw new Error('The call is not connected yet')
      const value = String(tone)
      if (!/^[0-9*#]$/.test(value)) throw new Error('Invalid DTMF key')
      if (dtmfFeedback) return false
      const frequencies = {
        '1': [697, 1209], '2': [697, 1336], '3': [697, 1477],
        '4': [770, 1209], '5': [770, 1336], '6': [770, 1477],
        '7': [852, 1209], '8': [852, 1336], '9': [852, 1477],
        '*': [941, 1209], '0': [941, 1336], '#': [941, 1477],
      }[value]
      const AudioContext = window.AudioContext || window.webkitAudioContext
      const context = AudioContext ? new AudioContext() : null
      let oscillators = []
      if (context) {
        const gain = context.createGain()
        gain.gain.value = 0.045
        oscillators = frequencies.map((frequency) => {
          const oscillator = context.createOscillator()
          oscillator.frequency.value = frequency
          oscillator.type = 'sine'
          oscillator.connect(gain)
          oscillator.start()
          return oscillator
        })
        gain.connect(context.destination)
        context.resume().catch(() => {})
      }
      dtmfFeedback = { tone: value, target, startedAt: performance.now(), context, oscillators }
      diagnose('dtmf.feedback-started', { tone: value, frequencies }, target)
      return true
    },
    stopDTMF(tone = '') {
      const feedback = dtmfFeedback
      if (!feedback || (tone && String(tone) !== feedback.tone)) return false
      dtmfFeedback = null
      for (const oscillator of feedback.oscillators) { try { oscillator.stop() } catch {} }
      feedback.context?.close?.().catch?.(() => {})
      const duration = Math.max(70, Math.min(6000, Math.round(performance.now() - feedback.startedAt)))
      if (feedback.target === session && feedback.target.isEstablished()) this.dtmf(feedback.tone, legToken(feedback.target), duration)
      diagnose('dtmf.feedback-stopped', { tone: feedback.tone, duration_ms: duration }, feedback.target)
      return true
    },
    stop() { diagnose('ua.stop'); stopRingback('ua-stop'); stopIncomingRing('ua-stop'); cancelDTMFFeedback(); microphoneInput.release(); stopLevelMeters(); microphoneMute.reset(session); for (const current of sessionsByLegToken.values()) { stopQuality(current); current.terminate() } sessionsByLegToken.clear(); if (ua) ua.stop(); if (incomingAudioContext) incomingAudioContext.close().catch(() => {}); incomingAudioContext = null; session = null; ua = null; telecomMuted = false; systemInactive = false; if (audioElement) audioElement.muted = false },
  }
}
