package com.viti.operator.voice

import android.content.Context
import android.net.Uri
import android.os.Build
import android.telecom.DisconnectCause
import androidx.annotation.RequiresApi
import androidx.core.telecom.CallAttributesCompat
import androidx.core.telecom.CallControlResult
import androidx.core.telecom.CallControlScope
import androidx.core.telecom.CallEndpointCompat
import androidx.core.telecom.CallsManager
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch

@RequiresApi(Build.VERSION_CODES.O)
class CoreTelecomGateway(context: Context) : PlatformCallGateway {
    private val callsManager = CallsManager(context.applicationContext).apply {
        registerAppWithTelecom(CallsManager.CAPABILITY_BASELINE)
    }

    override suspend fun runCall(
        request: PlatformCallRequest,
        callbacks: PlatformCallCallbacks,
        onReady: (PlatformCallControl) -> Unit,
    ) {
        val attributes = CallAttributesCompat(
            request.displayName,
            Uri.fromParts("sip", request.peer, null),
            if (request.direction == VoiceCallDirection.INCOMING) {
                CallAttributesCompat.DIRECTION_INCOMING
            } else {
                CallAttributesCompat.DIRECTION_OUTGOING
            },
            CallAttributesCompat.CALL_TYPE_AUDIO_CALL,
            CallAttributesCompat.SUPPORTS_SET_INACTIVE,
        )
        callsManager.addCall(
            attributes,
            onAnswer = { callbacks.onAnswer() },
            onDisconnect = { callbacks.onDisconnect(it.code == DisconnectCause.REJECTED) },
            onSetActive = { callbacks.onSetActive() },
            onSetInactive = { callbacks.onSetInactive() },
        ) {
            val endpoints = EndpointRegistry()
            val control = CorePlatformCallControl(this, endpoints)
            onReady(control)
            launch {
                combine(currentCallEndpoint, availableEndpoints, isMuted) { current, available, muted ->
                    endpoints.replace(available)
                    PlatformAudioState(
                        current = current.toVoiceEndpoint(),
                        available = available.map(CallEndpointCompat::toVoiceEndpoint),
                        telecomMuted = muted,
                    )
                }.collect(callbacks::onAudioState)
            }
        }
    }
}

@RequiresApi(Build.VERSION_CODES.O)
private class CorePlatformCallControl(
    private val scope: CallControlScope,
    private val endpoints: EndpointRegistry,
) : PlatformCallControl {
    override suspend fun answer() = scope.answer(CallAttributesCompat.CALL_TYPE_AUDIO_CALL).requireSuccess("answer")

    override suspend fun setActive() = scope.setActive().requireSuccess("activate")

    override suspend fun setInactive() = scope.setInactive().requireSuccess("make inactive")

    override suspend fun disconnect(rejected: Boolean) = scope.disconnect(
        DisconnectCause(if (rejected) DisconnectCause.REJECTED else DisconnectCause.LOCAL),
    ).requireSuccess("disconnect")

    override suspend fun requestEndpoint(endpointId: String) {
        val endpoint = endpoints.find(endpointId)
            ?: throw IllegalStateException("The selected audio endpoint is no longer available")
        scope.requestEndpointChange(endpoint).requireSuccess("change audio endpoint")
    }
}

@RequiresApi(Build.VERSION_CODES.O)
private class EndpointRegistry {
    @Volatile
    private var endpoints: List<CallEndpointCompat> = emptyList()

    fun replace(value: List<CallEndpointCompat>) {
        endpoints = value.toList()
    }

    fun find(id: String): CallEndpointCompat? = endpoints.firstOrNull { it.identifier.toString() == id }
}

private fun CallControlResult.requireSuccess(action: String) {
    if (this is CallControlResult.Error) {
        throw IllegalStateException("Telecom could not $action the call (error $errorCode)")
    }
}

@RequiresApi(Build.VERSION_CODES.O)
private fun CallEndpointCompat.toVoiceEndpoint(): VoiceAudioEndpoint = VoiceAudioEndpoint(
    id = identifier.toString(),
    name = name.toString(),
    type = when (type) {
        CallEndpointCompat.TYPE_EARPIECE -> VoiceAudioEndpointType.EARPIECE
        CallEndpointCompat.TYPE_SPEAKER -> VoiceAudioEndpointType.SPEAKER
        CallEndpointCompat.TYPE_WIRED_HEADSET -> VoiceAudioEndpointType.WIRED_HEADSET
        CallEndpointCompat.TYPE_BLUETOOTH -> VoiceAudioEndpointType.BLUETOOTH
        CallEndpointCompat.TYPE_STREAMING -> VoiceAudioEndpointType.STREAMING
        else -> VoiceAudioEndpointType.UNKNOWN
    },
)
