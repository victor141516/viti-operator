export class LiveCallSoakTracker {
  constructor({ minCallSeconds = 7200, minIkeRenewals = 2, minChildRenewals = 2, postCallSeconds = 120 } = {}) {
    this.requirements = { minCallSeconds, minIkeRenewals, minChildRenewals, postCallSeconds }
    this.phase = 'waiting'
    this.callStartedAt = null
    this.callEndedAt = null
    this.activeSamples = 0
    this.unhealthySamples = []
    this.ikeIDs = new Set()
    this.childIDs = new Set()
    this.ikeRenewals = 0
    this.childRenewals = 0
    this.fatalReason = ''
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
    if (this.phase === 'waiting') {
      if (!active) return
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
    if (reasons.length) this.unhealthySamples.push({ timestamp: sample.timestamp, reasons })

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
      && this.unhealthySamples.length === 0
    return {
      accepted,
      phase: this.phase,
      fatal_reason: this.fatalReason || null,
      call_detected: this.callStartedAt !== null,
      call_duration_seconds: callDurationSeconds,
      active_samples: this.activeSamples,
      unhealthy_samples: this.unhealthySamples.length,
      observed_ike_renewals: this.ikeRenewals,
      observed_child_renewals: this.childRenewals,
      requirements: {
        minimum_call_seconds: this.requirements.minCallSeconds,
        minimum_ike_renewals: this.requirements.minIkeRenewals,
        minimum_child_renewals: this.requirements.minChildRenewals,
        post_call_seconds: this.requirements.postCallSeconds,
      },
    }
  }
}
