package com.viti.operator.voice

/** Only fixed signaling categories are retained; never copy an exception or SIP payload. */
internal class VoiceSignalingProbe(private val now: () -> Long) {
    private val startedAt = now()
    var tickCount = 0L
        private set
    var event = "none"
        private set
    var jsStatus = "none"
        private set
    private var eventAt: Long? = null
    private var jsStatusAt: Long? = null

    fun tick() { tickCount++ }
    fun observe(type: String, status: String = "") {
        if (type !in EVENTS) return
        event = type
        eventAt = now()
        if (type == "signaling-status") {
            jsStatusAt = now()
            jsStatus = status.takeIf { it in setOf("registered", "not_registered", "disabled") } ?: "none"
        }
    }
    fun uptimeMs() = (now() - startedAt).coerceAtLeast(0)
    fun eventAgeMs() = eventAt?.let { (now() - it).coerceAtLeast(0) } ?: -1L
    fun jsStatusAgeMs() = jsStatusAt?.let { (now() - it).coerceAtLeast(0) } ?: -1L

    companion object {
        private val EVENTS = setOf("connected", "disconnected", "registered", "unregistered", "registration-failed", "signaling-status", "maintenance", "maintenance-failed")
        const val BUILD = "native-voice-20261007"
    }
}
