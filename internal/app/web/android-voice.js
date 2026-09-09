(() => {
  let voice = null
  const emit = (type, detail = {}) => {
    if (window.VitiAndroid?.onVoiceEvent) {
      window.VitiAndroid.onVoiceEvent(JSON.stringify({ type, ...detail }))
    }
  }
  window.vitiVoiceStart = (provisioning) => {
    try {
      voice?.stop()
      voice = VitiVoiceBundle.createVoice(emit, document.querySelector('#remote-audio'))
      voice.start(JSON.parse(provisioning))
    } catch (error) {
      emit('failed', { cause: error?.message || 'Could not start telephony' })
    }
  }
  window.vitiVoiceCall = (number, traceId) => {
    try { voice?.call(number, traceId) } catch (error) { emit('failed', { trace_id: traceId, cause: error?.message || 'Could not place the call' }) }
  }
  window.vitiVoiceAnswer = () => {
    try { voice?.answer() } catch (error) { emit('failed', { cause: error?.message || 'Could not answer the call' }) }
  }
  window.vitiVoiceDtmf = (tone) => {
    try { voice?.dtmf(tone) } catch (error) { emit('dtmf-failed', { tone, cause: error?.message || 'Could not send the tone' }) }
  }
  window.vitiVoiceReject = () => voice?.reject()
  window.vitiVoiceHangup = () => voice?.hangup()
  window.vitiVoiceMute = (muted) => voice?.mute(Boolean(muted))
})()
