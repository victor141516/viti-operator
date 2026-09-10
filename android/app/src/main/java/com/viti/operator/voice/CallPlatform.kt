package com.viti.operator.voice

import com.viti.operator.data.VoiceProvisioning

enum class VoiceCallDirection { INCOMING, OUTGOING }

// PackageManager.FEATURE_TELECOM was only exposed as an SDK constant much
// later than the feature itself, so use its stable platform feature name.
internal const val TELECOM_FEATURE = "android.software.telecom"

enum class VoiceCallPhase { SETUP, RINGING, DIALING, ACTIVE, INACTIVE, RECONNECTING, ENDING }

enum class VoiceAudioEndpointType { EARPIECE, SPEAKER, WIRED_HEADSET, BLUETOOTH, STREAMING, UNKNOWN }

data class VoiceAudioEndpoint(
    val id: String,
    val name: String,
    val type: VoiceAudioEndpointType,
)

data class PlatformCallRequest(
    val token: String,
    val peer: String,
    val displayName: String,
    val direction: VoiceCallDirection,
)

data class PlatformAudioState(
    val current: VoiceAudioEndpoint? = null,
    val available: List<VoiceAudioEndpoint> = emptyList(),
    val telecomMuted: Boolean = false,
)

interface PlatformCallControl {
    suspend fun answer()
    suspend fun setActive()
    suspend fun setInactive()
    suspend fun disconnect(rejected: Boolean = false)
    suspend fun requestEndpoint(endpointId: String)
}

interface PlatformCallCallbacks {
    suspend fun onAnswer()
    suspend fun onDisconnect(rejected: Boolean)
    suspend fun onSetActive()
    suspend fun onSetInactive()
    suspend fun onAudioState(state: PlatformAudioState)
}

interface PlatformCallGateway {
    /** Suspends for the complete lifetime of the admitted Telecom call. */
    suspend fun runCall(
        request: PlatformCallRequest,
        callbacks: PlatformCallCallbacks,
        onReady: (PlatformCallControl) -> Unit,
    )
}

interface VoiceEngineControl {
    fun hasMicrophonePermission(): Boolean
    fun start(provisioning: VoiceProvisioning)
    suspend fun call(legToken: String, number: String, traceId: String)
    suspend fun answer(legToken: String)
    suspend fun reject(legToken: String)
    suspend fun hangup(legToken: String)
	suspend fun reconnect(legToken: String, traceId: String)
    suspend fun setMuted(legToken: String, muted: Boolean)
    suspend fun setTelecomMuted(legToken: String, muted: Boolean)
    suspend fun setSystemInactive(legToken: String, inactive: Boolean)
    suspend fun sendDtmf(legToken: String, tone: Char)
    fun destroy()
}

interface VoiceCallSessionControl {
	suspend fun isCarrierCallActive(traceId: String): Boolean
	suspend fun leave(traceId: String, connectionId: String, previousConnectionId: String, operationId: String)
}

object NoOpVoiceCallSessionControl : VoiceCallSessionControl {
	override suspend fun isCarrierCallActive(traceId: String) = false
	override suspend fun leave(traceId: String, connectionId: String, previousConnectionId: String, operationId: String) = Unit
}
