(() => {
  let voice = null
  const emit = (type, detail = {}) => VitiAndroid.onVoiceEvent(JSON.stringify({ type, ...detail }))
  window.vitiVoiceStart = (provisioning, identity) => {
    try {
      const configuration = JSON.parse(provisioning)
      voice?.stop()
      voice = null
      if (!configuration.enabled) {
        emit('disconnected')
        return
      }
      voice = VitiVoiceBundle.createVoice(emit, document.querySelector('#remote-audio'), JSON.parse(identity))
      voice.start(configuration)
    } catch (error) {
      emit('failed', { cause: error?.message || 'Could not start telephony' })
    }
  }
  window.vitiVoiceCommand = (payload) => {
    try {
      if (!voice) throw new Error('The voice engine is not ready')
      const request = JSON.parse(payload)
      const token = String(request.leg_token || '')
      const args = request.args || {}
      let result
      switch (request.action) {
        case 'call': result = voice.call(String(args.number || ''), String(args.trace_id || ''), token); break
        case 'answer': result = voice.answer(token); break
        case 'reject': result = voice.reject(token); break
        case 'hangup': result = voice.hangup(token); break
		case 'reconnect': result = voice.reconnect(String(args.trace_id || ''), token); break
        case 'mute': result = voice.mute(Boolean(args.muted), token); break
        case 'telecom-muted': result = voice.setTelecomMuted(Boolean(args.muted), token); break
        case 'inactive': result = voice.setSystemInactive(Boolean(args.inactive), token); break
        case 'dtmf': result = voice.dtmf(String(args.tone || ''), token); break
        default: throw new Error(`Unknown voice command: ${request.action}`)
      }
      return JSON.stringify({ ok: true, leg_token: token, result })
    } catch (error) {
      return JSON.stringify({ ok: false, error: error?.message || 'Voice command failed' })
    }
  }
})()
