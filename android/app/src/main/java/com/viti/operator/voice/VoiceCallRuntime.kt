package com.viti.operator.voice

import com.viti.operator.data.VoiceProvisioning
import com.viti.operator.data.VoiceUiState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import java.util.UUID

data class VoiceCallSnapshot(
    val voice: VoiceUiState = VoiceUiState(),
    val legToken: String = "",
    val direction: VoiceCallDirection? = null,
    val phase: VoiceCallPhase? = null,
    val peer: String = "",
    val displayName: String = "",
    val platformReady: Boolean = false,
    val telecomManaged: Boolean = false,
    val microphonePermissionRequired: Boolean = false,
    val audio: PlatformAudioState = PlatformAudioState(),
)

interface LegacyAudioControl {
    fun begin(direction: VoiceCallDirection)
    fun setActive(value: Boolean)
    fun requestEndpoint(endpointId: String)
    fun end()
    fun close()
}

/**
 * Serializes one local SIP leg with one Android call. All mutable state is
 * confined to the supplied main-thread service scope.
 */
class VoiceCallRuntime(
    private val scope: CoroutineScope,
    private val voiceClient: VoiceEngineControl,
    private val telecomGateway: PlatformCallGateway?,
    private val legacyAudio: LegacyAudioControl,
    private val onSnapshot: (VoiceCallSnapshot) -> Unit,
    private val onDiagnostic: (String, Map<String, Any?>) -> Unit = { _, _ -> },
) {
    private data class Leg(
        val token: String,
        val direction: VoiceCallDirection,
        val peer: String,
        val displayName: String,
        var phase: VoiceCallPhase,
        var control: PlatformCallControl? = null,
        var platformJob: Job? = null,
        var sipStarted: Boolean = false,
        var platformActive: Boolean = false,
        var platformDisconnecting: Boolean = false,
        var pendingAnswer: Boolean = false,
        var audio: PlatformAudioState = PlatformAudioState(),
    )

    private var leg: Leg? = null
    private var voice = VoiceUiState()
    private var deferredProvisioning: VoiceProvisioning? = null
    private val _snapshot = MutableStateFlow(VoiceCallSnapshot())
    val snapshot = _snapshot.asStateFlow()

    fun start(provisioning: VoiceProvisioning) {
        if (leg == null) {
            voiceClient.start(provisioning)
        } else {
            deferredProvisioning = provisioning
        }
    }

    fun placeCall(number: String, displayName: String = number) {
        val peer = number.trim()
        if (peer.isEmpty()) return
        if (leg != null) {
            publishNotice("Another call is already active on this device")
            return
        }
        if (!voiceClient.hasMicrophonePermission()) {
            publishNotice("Microphone access is required for calls")
            return
        }
        val created = Leg(
            token = UUID.randomUUID().toString(),
            direction = VoiceCallDirection.OUTGOING,
            peer = peer,
            displayName = displayName.ifBlank { peer },
            phase = VoiceCallPhase.SETUP,
        )
        leg = created
        voice = VoiceUiState(state = "calling", peer = peer, detail = "Preparing the call with Android…")
        publish()
        beginAdmission(created)
    }

    fun answer(expectedToken: String? = null) {
        val current = leg ?: return
        if (expectedToken != null && current.token != expectedToken) {
            onDiagnostic("telecom.stale-action", mapOf("action" to "answer", "leg_token" to expectedToken))
            return
        }
        if (current.direction != VoiceCallDirection.INCOMING) return
        if (!voiceClient.hasMicrophonePermission()) {
            current.pendingAnswer = true
            publish(permissionRequired = true)
            return
        }
        if (telecomGateway != null && current.control == null) {
            current.pendingAnswer = true
            publish(permissionRequired = false)
            if (current.platformJob == null) beginTelecomAdmission(current)
            return
        }
        scope.launch { runUserAction("answer", current.token) { answer(current.token, fromPlatform = false) } }
    }

    fun reject(expectedToken: String? = null) {
        val current = leg ?: return
        if (expectedToken != null && current.token != expectedToken) {
            onDiagnostic("telecom.stale-action", mapOf("action" to "reject", "leg_token" to expectedToken))
            return
        }
        scope.launch {
            runUserAction("reject", current.token) {
                leave(current.token, rejected = current.direction == VoiceCallDirection.INCOMING, fromPlatform = false)
            }
        }
    }

    fun hangup(expectedToken: String? = null) {
        val current = leg ?: return
        if (expectedToken != null && current.token != expectedToken) {
            onDiagnostic("telecom.stale-action", mapOf("action" to "hangup", "leg_token" to expectedToken))
            return
        }
        scope.launch { runUserAction("hangup", current.token) { leave(current.token, rejected = false, fromPlatform = false) } }
    }

    fun setMuted(muted: Boolean) {
        val current = leg ?: return
        scope.launch {
            runCommand(current.token, "mute") { voiceClient.setMuted(current.token, muted) }
        }
    }

    fun sendDtmf(tone: Char) {
        val current = leg ?: return
        scope.launch { runCommand(current.token, "dtmf") { voiceClient.sendDtmf(current.token, tone) } }
    }

    fun requestEndpoint(endpointId: String) {
        val current = leg ?: return
        scope.launch {
            runCatching {
                current.control?.requestEndpoint(endpointId)
                    ?: if (telecomGateway == null) {
                        legacyAudio.requestEndpoint(endpointId)
                    } else {
                        error("Android Telecom has not exposed audio routes yet")
                    }
            }.onFailure { publishNotice(it.message ?: "Could not change the audio route") }
        }
    }

    fun onLegacyAudioState(state: PlatformAudioState) {
        if (telecomGateway != null) return
        val current = leg ?: return
        current.audio = state
        publish()
    }

    fun onLegacyInactiveChanged(inactive: Boolean) {
        if (telecomGateway != null) return
        val current = leg ?: return
        scope.launch {
            runCatching { voiceClient.setSystemInactive(current.token, inactive) }
                .onSuccess {
                    current.phase = if (inactive) VoiceCallPhase.INACTIVE else VoiceCallPhase.ACTIVE
                    publish()
                }
                .onFailure { publishNotice(it.message ?: "Could not update call audio focus") }
        }
    }

    fun onVoiceEvent(event: VoiceEngineEvent) {
        if (event.type == "incoming") {
            acceptIncomingEvent(event)
            return
        }
        if (event.legToken.isNotBlank()) {
            val current = leg
            if (current == null || current.token != event.legToken) {
                onDiagnostic("telecom.stale-voice-event", mapOf("type" to event.type, "leg_token" to event.legToken))
                return
            }
        }
        when (event.type) {
            "connected" -> if (leg == null) voice = VoiceUiState(state = "connected", detail = "Connecting the line…")
            "registered" -> if (leg == null) voice = VoiceUiState(state = "registered", detail = "Line ready for calls")
            "calling" -> updateLegPhase(event, VoiceCallPhase.DIALING, "Calling ${event.peer.ifBlank { leg?.peer.orEmpty() }}…")
            "ringing" -> updateLegPhase(event, VoiceCallPhase.RINGING, "Ringing…")
            "active" -> activateFromSip(event)
            "media" -> updateVoice(event, if ("sent=true" in event.cause && "received=true" in event.cause) "Call in progress · two-way audio" else "Call in progress")
            "mute-changed" -> {
                voice = voice.copy(microphoneMuted = event.cause == "muted=true")
                publish()
            }
            "dtmf" -> Unit
            "dtmf-failed" -> publishNotice(event.cause.ifBlank { "Could not send the DTMF tone" })
            "ended", "failed" -> finishFromSip(event)
            "registration-failed" -> if (leg == null) voice = VoiceUiState(state = event.type, detail = event.cause.ifBlank { "Could not register the line" })
            "disconnected" -> if (leg == null) voice = VoiceUiState(state = "disconnected", detail = "Telephony disconnected")
        }
        if (event.type in setOf("connected", "registered", "registration-failed", "disconnected")) publish()
    }

    fun close() {
        val current = leg
        current?.platformJob?.cancel()
        legacyAudio.close()
        voiceClient.destroy()
    }

    private fun acceptIncomingEvent(event: VoiceEngineEvent) {
        if (event.legToken.isBlank()) {
            onDiagnostic("telecom.incoming-without-token", emptyMap())
            return
        }
        val existing = leg
        if (existing != null) {
            if (existing.token != event.legToken) {
                scope.launch { runCatching { voiceClient.reject(event.legToken) } }
            }
            return
        }
        val created = Leg(
            token = event.legToken,
            direction = VoiceCallDirection.INCOMING,
            peer = event.peer,
            displayName = event.peer.ifBlank { "Private number" },
            phase = VoiceCallPhase.RINGING,
            sipStarted = true,
        )
        leg = created
        voice = VoiceUiState(state = "incoming", peer = event.peer, detail = "Incoming call from ${event.peer}")
        val needsPermission = !voiceClient.hasMicrophonePermission()
        publish(permissionRequired = needsPermission)
        if (!needsPermission) beginAdmission(created)
    }

    private fun beginAdmission(current: Leg) {
        if (telecomGateway == null) {
            legacyAudio.begin(current.direction)
            current.control = null
            if (current.direction == VoiceCallDirection.OUTGOING) {
                scope.launch { dialSip(current.token) }
            }
            publish()
        } else {
            beginTelecomAdmission(current)
        }
    }

    private fun beginTelecomAdmission(current: Leg) {
        if (current.platformJob != null) return
        val gateway = telecomGateway ?: return
        current.platformJob = scope.launch {
            var ready = false
            try {
                gateway.runCall(
                    PlatformCallRequest(current.token, current.peer, current.displayName, current.direction),
                    callbacksFor(current.token),
                ) { control ->
                    val live = leg
                    if (live == null || live.token != current.token || live.phase == VoiceCallPhase.ENDING) {
                        scope.launch { runCatching { control.disconnect() } }
                        return@runCall
                    }
                    ready = true
                    live.control = control
                    publish()
                    if (live.direction == VoiceCallDirection.OUTGOING) {
                        scope.launch { dialSip(live.token) }
                    } else if (live.pendingAnswer) {
                        scope.launch { answer(live.token, fromPlatform = false) }
                    }
                }
                val live = leg
                if (live?.token == current.token && live.phase != VoiceCallPhase.ENDING) {
                    if (live.sipStarted) runCatching { voiceClient.hangup(live.token) }
                    failAndClear(live.token, "Android ended its call scope")
                }
            } catch (cancelled: CancellationException) {
                throw cancelled
            } catch (error: Throwable) {
                val live = leg
                if (live?.token != current.token) return@launch
                onDiagnostic("telecom.add-call-failed", mapOf("ready" to ready, "error" to error.toString()))
                if (live.sipStarted) runCatching { voiceClient.reject(live.token) }
                failAndClear(live.token, error.message ?: "Android rejected the call")
            }
        }
    }

    private fun callbacksFor(token: String) = object : PlatformCallCallbacks {
        override suspend fun onAnswer() = answer(token, fromPlatform = true)

        override suspend fun onDisconnect(rejected: Boolean) = leave(token, rejected, fromPlatform = true)

        override suspend fun onSetActive() {
            val current = requireCurrent(token)
            voiceClient.setSystemInactive(token, false)
            current.phase = VoiceCallPhase.ACTIVE
            current.platformActive = true
            publish()
        }

        override suspend fun onSetInactive() {
            val current = requireCurrent(token)
            voiceClient.setSystemInactive(token, true)
            current.phase = VoiceCallPhase.INACTIVE
            current.platformActive = false
            publish()
        }

        override suspend fun onAudioState(state: PlatformAudioState) {
            val current = requireCurrent(token)
            if (current.audio.telecomMuted != state.telecomMuted) {
                voiceClient.setTelecomMuted(token, state.telecomMuted)
            }
            current.audio = state
            publish()
        }
    }

    private suspend fun dialSip(token: String) {
        val current = leg?.takeIf { it.token == token } ?: return
        if (current.sipStarted) return
        try {
            voiceClient.call(token, current.peer, UUID.randomUUID().toString())
            current.sipStarted = true
            current.phase = VoiceCallPhase.DIALING
            publish()
        } catch (error: Throwable) {
            current.phase = VoiceCallPhase.ENDING
            val control = current.control
            failAndClear(token, error.message ?: "Could not place the call")
            control?.let { runCatching { it.disconnect() } }
        }
    }

    private suspend fun answer(token: String, fromPlatform: Boolean) {
        val current = requireCurrent(token)
        if (!voiceClient.hasMicrophonePermission()) throw IllegalStateException("Microphone access is required to answer")
        try {
            // Accept the service leg before claiming success to Telecom. If
            // Telecom then rejects the transition, roll the service leg back.
            voiceClient.answer(token)
            if (!fromPlatform) current.control?.answer()
            current.pendingAnswer = false
            current.platformActive = true
            current.phase = VoiceCallPhase.ACTIVE
            if (telecomGateway == null) legacyAudio.setActive(true)
            voice = voice.copy(state = "active", detail = "Call in progress")
            publish()
        } catch (error: Throwable) {
            runCatching { voiceClient.hangup(token) }
            current.phase = VoiceCallPhase.ENDING
            val control = current.control
            failAndClear(token, error.message ?: "Could not answer the call")
            if (!fromPlatform) runCatching { control?.disconnect() }
            throw error
        }
    }

    private suspend fun leave(token: String, rejected: Boolean, fromPlatform: Boolean) {
        val current = requireCurrent(token)
        current.phase = VoiceCallPhase.ENDING
        publish()
        var failure: Throwable? = null
        try {
            if (rejected && current.direction == VoiceCallDirection.INCOMING) voiceClient.reject(token) else voiceClient.hangup(token)
        } catch (error: Throwable) {
            failure = error
        }
        if (!fromPlatform) {
            current.platformDisconnecting = true
            try {
                current.control?.disconnect(rejected)
            } catch (error: Throwable) {
                failure?.addSuppressed(error) ?: run { failure = error }
            }
        }
        clearLeg(token, if (rejected) "Call rejected" else "You left the call", failed = failure != null)
        failure?.let { throw it }
    }

    private suspend fun runUserAction(action: String, token: String, block: suspend () -> Unit) {
        runCatching { block() }.onFailure { error ->
            if (error is CancellationException) throw error
            onDiagnostic(
                "telecom.user-action-failed",
                mapOf("action" to action, "leg_token" to token, "error" to error.toString()),
            )
            if (leg?.token == token) failAndClear(token, error.message ?: "The call action failed")
        }
    }

    private fun activateFromSip(event: VoiceEngineEvent) {
        val current = leg ?: return
        current.phase = VoiceCallPhase.ACTIVE
        if (telecomGateway == null) legacyAudio.setActive(true)
        voice = voice.copy(state = "active", peer = event.peer.ifBlank { current.peer }, detail = "Call in progress")
        publish()
        if (current.control != null && !current.platformActive) {
            scope.launch {
                runCatching { current.control?.setActive() }
                    .onSuccess { current.platformActive = true; publish() }
                    .onFailure {
                        runCatching { voiceClient.hangup(current.token) }
                        failAndClear(current.token, it.message ?: "Android could not activate the call")
                    }
            }
        }
    }

    private fun updateLegPhase(event: VoiceEngineEvent, phase: VoiceCallPhase, detail: String) {
        val current = leg ?: return
        current.phase = phase
        updateVoice(event, detail)
    }

    private fun updateVoice(event: VoiceEngineEvent, detail: String) {
        val current = leg ?: return
        voice = voice.copy(state = event.type, peer = event.peer.ifBlank { current.peer }, detail = detail)
        publish()
    }

    private fun finishFromSip(event: VoiceEngineEvent) {
        val current = leg ?: return
        val token = current.token
        scope.launch {
            if (!current.platformDisconnecting) {
                current.platformDisconnecting = true
                runCatching { current.control?.disconnect() }
            }
            clearLeg(token, if (event.type == "failed") event.cause.ifBlank { "The call failed" } else "Call ended")
        }
    }

    private suspend fun runCommand(token: String, kind: String, command: suspend () -> Unit) {
        requireCurrent(token)
        runCatching { command() }
            .onFailure {
                onDiagnostic("telecom.command-failed", mapOf("command" to kind, "leg_token" to token, "error" to it.toString()))
                publishNotice(it.message ?: "The call command failed")
            }
    }

    private fun requireCurrent(token: String): Leg = leg?.takeIf { it.token == token }
        ?: throw VoiceCommandException("This call leg is no longer current")

    private fun failAndClear(token: String, detail: String) {
        clearLeg(token, "The call failed: $detail", failed = true)
    }

    private fun clearLeg(token: String, detail: String, failed: Boolean = false) {
        val current = leg?.takeIf { it.token == token } ?: return
        // A Core-Telecom addCall coroutine owns the platform scope and returns
        // naturally after disconnect. Cancelling it here may cancel the very
        // callback that is completing this teardown.
        leg = null
        if (telecomGateway == null) legacyAudio.end()
        voice = VoiceUiState(state = "registered", detail = detail)
        publish()
        deferredProvisioning?.let { provisioning ->
            deferredProvisioning = null
            voiceClient.start(provisioning)
        }
        onDiagnostic("telecom.call-cleared", mapOf("leg_token" to token, "failed" to failed, "detail" to detail))
    }

    private fun publish(permissionRequired: Boolean = false) {
        val current = leg
        val value = VoiceCallSnapshot(
            voice = voice,
            legToken = current?.token.orEmpty(),
            direction = current?.direction,
            phase = current?.phase,
            peer = current?.peer.orEmpty(),
            displayName = current?.displayName.orEmpty(),
            platformReady = current?.control != null || (current != null && telecomGateway == null),
            telecomManaged = current?.control != null,
            microphonePermissionRequired = permissionRequired,
            audio = current?.audio ?: PlatformAudioState(),
        )
        _snapshot.value = value
        onSnapshot(value)
    }

    private fun publishNotice(detail: String) {
        voice = voice.copy(detail = detail)
        publish(permissionRequired = leg != null && !voiceClient.hasMicrophonePermission())
    }
}
