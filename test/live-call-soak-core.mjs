export class LiveCallSoakTracker {
  constructor({
    minCallSeconds = 7200,
    minIkeRenewals = 2,
    minChildRenewals = 2,
    postCallSeconds = 120,
    maxMediaStallSeconds = 15,
  } = {}) {
    this.requirements = { minCallSeconds, minIkeRenewals, minChildRenewals, postCallSeconds, maxMediaStallSeconds }
    this.phase = 'waiting'
    this.callStartedAt = null
    this.callEndedAt = null
    this.activeSamples = 0
    this.unhealthySamples = []
    this.ikeIDs = new Set()
    this.childIDs = new Set()
    this.ikeRenewals = 0
    this.childRenewals = 0
    this.mediaStreams = new Map()
    this.mediaRxProgressSamples = 0
    this.mediaTxProgressSamples = 0
    this.maxMediaRxStallSeconds = 0
    this.maxMediaTxStallSeconds = 0
    this.fatalReason = ''
  }

  observeMediaDirection(stream, direction, packets, at, reasons) {
    const count = Number(packets)
    const lastCountKey = direction === 'rx' ? 'lastRxPackets' : 'lastTxPackets'
    const lastProgressKey = direction === 'rx' ? 'lastRxProgressAt' : 'lastTxProgressAt'
    const progressSamplesKey = direction === 'rx' ? 'mediaRxProgressSamples' : 'mediaTxProgressSamples'
    const maxStallKey = direction === 'rx' ? 'maxMediaRxStallSeconds' : 'maxMediaTxStallSeconds'
    if (!Number.isFinite(count) || count < 0) {
      reasons.push(`media_${direction}_unavailable`)
      return
    }
    if (stream[lastCountKey] === null || count !== stream[lastCountKey]) {
      if (stream[lastCountKey] !== null) this[progressSamplesKey] += 1
      stream[lastCountKey] = count
      stream[lastProgressKey] = at
      return
    }
    const stalledSeconds = Math.max(0, (at - stream[lastProgressKey]) / 1000)
    this[maxStallKey] = Math.max(this[maxStallKey], stalledSeconds)
    if (stalledSeconds > this.requirements.maxMediaStallSeconds) {
      reasons.push(`media_${direction}_stalled`)
    }
  }

  observeMediaStreams(streams, at, reasons) {
    if (!Array.isArray(streams) || streams.length < 1) {
      reasons.push('media_channels_absent')
      return
    }
    const current = new Set()
    for (const item of streams) {
      const id = String(item?.id || '')
      if (!id || current.has(id)) {
        reasons.push('media_stream_identity_invalid')
        continue
      }
      current.add(id)
      let stream = this.mediaStreams.get(id)
      if (!stream) {
        stream = { lastRxPackets: null, lastTxPackets: null, lastRxProgressAt: null, lastTxProgressAt: null }
        this.mediaStreams.set(id, stream)
      }
      this.observeMediaDirection(stream, 'rx', item.rx_packets, at, reasons)
      this.observeMediaDirection(stream, 'tx', item.tx_packets, at, reasons)
    }
    for (const id of this.mediaStreams.keys()) {
      if (!current.has(id)) {
        reasons.push('media_stream_removed')
        this.mediaStreams.delete(id)
      }
    }
  }

  observe(sample) {
    const at = Date.parse(sample.timestamp)
    if (!Number.isFinite(at)) throw new Error('sample timestamp is invalid')
    if (this.phase === 'complete') return
    if (sample.snapshot_ok === false) {
      if (this.phase === 'active') this.unhealthySamples.push({ timestamp: sample.timestamp, reasons: ['snapshot_failed'] })
      return
    }

    const active = Number(sample.active_channels) > 0
    const answered = Number(sample.conference_count) === 1 && Number(sample.conference_parties) >= 2
    if (this.phase === 'waiting') {
      // Outbound ringing and 183 early media already create PJSIP channels and
      // RTP counters.  Only the post-answer ConfBridge proves that this is an
      // established call whose duration can satisfy the renewal soak.
      if (!answered) return
      this.phase = 'active'
      this.callStartedAt = at
      for (const id of sample.ike_ids || []) this.ikeIDs.add(String(id))
      for (const id of sample.child_ids || []) this.childIDs.add(String(id))
    } else if (this.phase === 'active' && !active) {
      this.phase = 'post-call'
      this.callEndedAt = at
      return
    } else if (this.phase === 'post-call') {
      if (active) {
        this.fatalReason = 'another call started during the post-call observation window'
        this.phase = 'complete'
        return
      }
      if (at - this.callEndedAt >= this.requirements.postCallSeconds * 1000) this.phase = 'complete'
      return
    }

    if (this.phase !== 'active') return
    this.activeSamples += 1
    const reasons = []
    if (!sample.charon) reasons.push('charon_absent')
    if (sample.python) reasons.push('python_backend_present')
    if (!sample.tunnel) reasons.push('tunnel_absent')
    if (!sample.ims) reasons.push('ims_unregistered')
    if (!(sample.ike_ids || []).length) reasons.push('ike_sa_absent')
    if (!(sample.child_ids || []).length) reasons.push('child_sa_absent')
    this.observeMediaStreams(sample.media_streams, at, reasons)
    if (reasons.length) this.unhealthySamples.push({ timestamp: sample.timestamp, reasons: [...new Set(reasons)] })

    for (const id of sample.ike_ids || []) {
      const value = String(id)
      if (!this.ikeIDs.has(value)) {
        this.ikeIDs.add(value)
        this.ikeRenewals += 1
      }
    }
    for (const id of sample.child_ids || []) {
      const value = String(id)
      if (!this.childIDs.has(value)) {
        this.childIDs.add(value)
        this.childRenewals += 1
      }
    }
  }

  finish(reason) {
    if (reason && !this.fatalReason) this.fatalReason = reason
    this.phase = 'complete'
  }

  summary() {
    const callDurationSeconds = this.callStartedAt !== null && this.callEndedAt !== null
      ? Math.max(0, (this.callEndedAt - this.callStartedAt) / 1000)
      : 0
    const accepted = this.phase === 'complete'
      && !this.fatalReason
      && this.callStartedAt !== null
      && this.callEndedAt !== null
      && callDurationSeconds >= this.requirements.minCallSeconds
      && this.ikeRenewals >= this.requirements.minIkeRenewals
      && this.childRenewals >= this.requirements.minChildRenewals
      && this.mediaRxProgressSamples > 0
      && this.mediaTxProgressSamples > 0
      && this.unhealthySamples.length === 0
    return {
      accepted,
      phase: this.phase,
      fatal_reason: this.fatalReason || null,
      call_detected: this.callStartedAt !== null,
      answered_call_detected: this.callStartedAt !== null,
      call_duration_seconds: callDurationSeconds,
      active_samples: this.activeSamples,
      unhealthy_samples: this.unhealthySamples.length,
      observed_ike_renewals: this.ikeRenewals,
      observed_child_renewals: this.childRenewals,
      media_rx_progress_samples: this.mediaRxProgressSamples,
      media_tx_progress_samples: this.mediaTxProgressSamples,
      maximum_observed_media_rx_stall_seconds: this.maxMediaRxStallSeconds,
      maximum_observed_media_tx_stall_seconds: this.maxMediaTxStallSeconds,
      requirements: {
        minimum_call_seconds: this.requirements.minCallSeconds,
        minimum_ike_renewals: this.requirements.minIkeRenewals,
        minimum_child_renewals: this.requirements.minChildRenewals,
        post_call_seconds: this.requirements.postCallSeconds,
        maximum_media_stall_seconds: this.requirements.maxMediaStallSeconds,
      },
    }
  }
}
