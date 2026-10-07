package com.viti.operator.voice

/** A slow WebView callback is not proof that the server removed a SIP contact. */
internal class VoiceRegistrationLease(private val now: () -> Long = System::currentTimeMillis) {
    private var expiresAtMs = 0L

    fun confirmed(expiresAt: Long) { expiresAtMs = expiresAt }
    fun lost() { expiresAtMs = 0L }
    fun isValid(): Boolean = expiresAtMs > now()
    fun hasExpired(): Boolean = expiresAtMs > 0 && !isValid()
    fun remainingMs(): Long = (expiresAtMs - now()).coerceAtLeast(0)
}
