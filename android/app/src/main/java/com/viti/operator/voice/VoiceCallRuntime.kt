package com.viti.operator.voice

import com.viti.operator.data.VoiceProvisioning
import com.viti.operator.data.VoiceUiState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeout
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
    val endingRejected: Boolean = false,
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
    private enum class LegAction { NONE, ANSWERING, LEAVING }

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
        var platformActivating: Boolean = false,
        var platformInactive: Boolean = false,
        var platformDisconnecting: Boolean = false,
        var pendingAnswer: Boolean = false,
        var action: LegAction = LegAction.NONE,
        var terminalDetail: String? = null,
        var terminalFailed: Boolean = false,
        var terminalRejected: Boolean = false,
        var legacyFocusGeneration: Long = 0,
        var audio: PlatformAudioState = PlatformAudioState(),
    )

    private var leg: Leg? = null
    private var voice = VoiceUiState()
    private var deferredProvisioning: VoiceProvisioning? = null
    private val legacyFocusMutex = Mutex()
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
        if (current.phase == VoiceCallPhase.ENDING) {
            publishNotice("The call is ending; hang up again to retry Android cleanup")
            return
        }
        if (current.direction != VoiceCallDirection.INCOMING) return
        if (!voiceClient.hasMicrophonePermission()) {
            current.pendingAnswer = true
            publish()
            return
        }
        if (!claimAction(current, LegAction.ANSWERING, "answer")) return
        if (telecomGateway != null && current.control == null) {
            current.pendingAnswer = true
            publish()
            return
        }
        scope.launch {
            try {
                runUserAction("answer", current.token) { performAnswer(current, fromPlatform = false) }
            } finally {
                releaseAction(current, LegAction.ANSWERING)
            }
        }
    }

    fun reject(expectedToken: String? = null) {
        val current = leg ?: return
        if (expectedToken != null && current.token != expectedToken) {
            onDiagnostic("telecom.stale-action", mapOf("action" to "reject", "leg_token" to expectedToken))
            return
        }
        if (current.direction != VoiceCallDirection.INCOMING) return
        if (current.phase != VoiceCallPhase.RINGING && !(current.phase == VoiceCallPhase.ENDING && current.terminalRejected)) {
            onDiagnostic("telecom.stale-action", mapOf("action" to "reject", "leg_token" to current.token))
            return
        }
        if (!claimAction(current, LegAction.LEAVING, "reject")) return
        scope.launch {
            try {
                runUserAction("reject", current.token) {
                    performLeave(current, rejected = true, fromPlatform = false)
                }
            } finally {
                releaseAction(current, LegAction.LEAVING)
            }
        }
    }

    fun hangup(expectedToken: String? = null) {
        val current = leg ?: return
        if (expectedToken != null && current.token != expectedToken) {
            onDiagnostic("telecom.stale-action", mapOf("action" to "hangup", "leg_token" to expectedToken))
            return
        }
        if (!claimAction(current, LegAction.LEAVING, "hangup")) return
        scope.launch {
            try {
                runUserAction("hangup", current.token) { performLeave(current, rejected = false, fromPlatform = false) }
            } finally {
                releaseAction(current, LegAction.LEAVING)
            }
        }
    }

    fun setMuted(muted: Boolean) {
        val current = leg ?: return
        if (current.phase == VoiceCallPhase.ENDING) return
        scope.launch {
            runCommand(current.token, "mute") { voiceClient.setMuted(current.token, muted) }
        }
    }

    fun sendDtmf(tone: Char) {
        val current = leg ?: return
        if (current.phase == VoiceCallPhase.ENDING) return
        scope.launch { runCommand(current.token, "dtmf") { voiceClient.sendDtmf(current.token, tone) } }
    }

    fun requestEndpoint(endpointId: String) {
        val current = leg ?: return
        if (current.phase == VoiceCallPhase.ENDING) return
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
        if (current.phase == VoiceCallPhase.ENDING) return
        current.audio = state
        publish()
    }

    fun onLegacyInactiveChanged(inactive: Boolean) {
        if (telecomGateway != null) return
        val current = leg ?: return
        if (current.phase == VoiceCallPhase.ENDING) return
        current.legacyFocusGeneration += 1
        val generation = current.legacyFocusGeneration
        if (inactive) {
            current.platformInactive = true
            current.platformActive = false
            current.phase = VoiceCallPhase.INACTIVE
            publish()
        }
        scope.launch {
            legacyFocusMutex.withLock {
                if (!isCurrentLegacyFocus(current, generation)) return@withLock
                try {
                    voiceClient.setSystemInactive(current.token, inactive)
                    if (!isCurrentLegacyFocus(current, generation)) return@withLock
                    current.platformInactive = inactive
                    current.phase = if (inactive) VoiceCallPhase.INACTIVE else VoiceCallPhase.ACTIVE
                    publish()
                } catch (error: Throwable) {
                    if (!isCurrentLegacyFocus(current, generation)) return@withLock
                    if (inactive) {
                        // Audio focus has already been lost. If the WebRTC leg
                        // cannot be suppressed, retaining it would leak call
                        // audio into a native call, so terminate this branch.
                        runCatching { voiceClient.hangup(current.token) }
                        terminateAfterEngineFailure(current, error.message ?: "Could not suppress call audio")
                    } else {
                        publishNotice(error.message ?: "Could not restore call audio focus")
                    }
                }
            }
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
        if (leg?.phase == VoiceCallPhase.ENDING && event.type in NON_TERMINAL_CALL_EVENTS) {
            onDiagnostic("telecom.event-during-ending", mapOf("type" to event.type, "leg_token" to event.legToken))
            return
        }
        when (event.type) {
            "connected" -> if (leg == null) voice = VoiceUiState(state = "connected", detail = "Connecting the line…")
            "registered" -> if (leg == null) voice = VoiceUiState(state = "registered", detail = "Line ready for calls")
            "calling" -> updateLegPhase(event, VoiceCallPhase.DIALING, "Calling ${event.peer.ifBlank { leg?.peer.orEmpty() }}…")
            "ringing" -> updateLegPhase(event, VoiceCallPhase.RINGING, "Ringing…")
            "active" -> activateFromSip(event)
            "media" -> updateVoice(event, if ("sent=true" in event.cause && "received=true" in event.cause) "Call in progress · two-way audio" else "Call in progress")
            "quality" -> {
                val quality = event.quality
                if (quality != null && quality.sequence > (voice.quality?.sequence ?: 0)) {
                    voice = voice.copy(quality = quality)
                    publish()
                }
            }
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
        publish()
        beginAdmission(created)
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
                        scope.launch {
                            try {
                                runUserAction("answer", live.token) { performAnswer(live, fromPlatform = false) }
                            } finally {
                                releaseAction(live, LegAction.ANSWERING)
                            }
                        }
                    }
                }
                val live = leg?.takeIf { it.token == current.token }
                if (live != null && live.terminalDetail == null) {
                    if (live.sipStarted) runCatching { voiceClient.hangup(live.token) }
                    markTerminal(live, "Android ended its call scope", failed = true)
                }
            } catch (cancelled: CancellationException) {
                throw cancelled
            } catch (error: Throwable) {
                val live = leg
                if (live?.token != current.token) return@launch
                onDiagnostic("telecom.add-call-failed", mapOf("ready" to ready, "error" to error.toString()))
                if (live.terminalDetail == null) {
                    if (live.sipStarted) {
                        runCatching {
                            if (live.direction == VoiceCallDirection.INCOMING) voiceClient.reject(live.token)
                            else voiceClient.hangup(live.token)
                        }
                    }
                    markTerminal(live, error.message ?: "Android rejected the call", failed = true)
                }
            } finally {
                val live = leg?.takeIf { it.token == current.token }
                if (live != null) {
                    val detail = live.terminalDetail ?: "Android ended its call scope"
                    clearLeg(current.token, detail, failed = live.terminalFailed || live.terminalDetail == null, platformScopeFinished = true)
                }
            }
        }
    }

    private fun callbacksFor(token: String) = object : PlatformCallCallbacks {
        override suspend fun onAnswer() {
            val current = leg?.takeIf { it.token == token } ?: return
            if (current.phase == VoiceCallPhase.ENDING) return
            if (!voiceClient.hasMicrophonePermission()) {
                val error = IllegalStateException("Microphone access is required to answer")
                schedulePlatformCleanup(token, rejected = true, detail = error.message.orEmpty())
                throw error
            } else {
                if (!claimAction(current, LegAction.ANSWERING, "platform-answer")) return
                try {
                    withTimeout(PLATFORM_CALLBACK_TIMEOUT_MS) { performAnswer(current, fromPlatform = true) }
                } catch (error: Throwable) {
                    schedulePlatformCleanup(token, rejected = false, detail = error.message ?: "Could not answer the call")
                    throw error
                } finally {
                    releaseAction(current, LegAction.ANSWERING)
                }
            }
        }

        override suspend fun onDisconnect(rejected: Boolean) {
            val current = leg?.takeIf { it.token == token } ?: return
            if (!claimAction(current, LegAction.LEAVING, if (rejected) "platform-reject" else "platform-disconnect")) return
            try {
                withTimeout(PLATFORM_CALLBACK_TIMEOUT_MS) {
                    performLeave(current, rejected, fromPlatform = true)
                }
            } finally {
                releaseAction(current, LegAction.LEAVING)
            }
        }

        override suspend fun onSetActive() {
            val current = requireCurrent(token)
            if (current.phase == VoiceCallPhase.ENDING) return
            withTimeout(PLATFORM_CALLBACK_TIMEOUT_MS) { voiceClient.setSystemInactive(token, false) }
            if (leg !== current || current.phase == VoiceCallPhase.ENDING) return
            current.platformInactive = false
            current.phase = VoiceCallPhase.ACTIVE
            current.platformActive = true
            publish()
        }

        override suspend fun onSetInactive() {
            val current = requireCurrent(token)
            if (current.phase == VoiceCallPhase.ENDING) return
            current.platformInactive = true
            current.phase = VoiceCallPhase.INACTIVE
            current.platformActive = false
            publish()
            try {
                withTimeout(PLATFORM_CALLBACK_TIMEOUT_MS) { voiceClient.setSystemInactive(token, true) }
            } catch (error: Throwable) {
                schedulePlatformCleanup(token, rejected = false, detail = error.message ?: "Could not make the call inactive")
                throw error
            }
        }

        override suspend fun onAudioState(state: PlatformAudioState) {
            val current = requireCurrent(token)
            if (current.phase == VoiceCallPhase.ENDING) return
            if (current.audio.telecomMuted != state.telecomMuted) {
                voiceClient.setTelecomMuted(token, state.telecomMuted)
            }
            if (leg !== current || current.phase == VoiceCallPhase.ENDING) return
            current.audio = state
            publish()
        }
    }

    private suspend fun dialSip(token: String) {
        val current = leg?.takeIf { it.token == token } ?: return
        if (current.sipStarted) return
        try {
            voiceClient.call(token, current.peer, UUID.randomUUID().toString())
            if (leg !== current || current.phase == VoiceCallPhase.ENDING) {
                runCatching { voiceClient.hangup(token) }
                return
            }
            current.sipStarted = true
            current.phase = VoiceCallPhase.DIALING
            publish()
        } catch (error: Throwable) {
            if (leg !== current || current.phase == VoiceCallPhase.ENDING) return
            current.phase = VoiceCallPhase.ENDING
            terminateAfterEngineFailure(current, error.message ?: "Could not place the call")
        }
    }

    private suspend fun performAnswer(current: Leg, fromPlatform: Boolean) {
        val token = current.token
        if (leg !== current || current.action != LegAction.ANSWERING || current.phase == VoiceCallPhase.ENDING) return
        if (!voiceClient.hasMicrophonePermission()) throw IllegalStateException("Microphone access is required to answer")
        try {
            // Accept the service leg before claiming success to Telecom. If
            // Telecom then rejects the transition, roll the service leg back.
            voiceClient.answer(token)
            if (leg !== current || current.phase == VoiceCallPhase.ENDING || current.action != LegAction.ANSWERING) {
                runCatching { voiceClient.hangup(token) }
                return
            }
            if (!fromPlatform) current.control?.answer()
            if (leg !== current || current.phase == VoiceCallPhase.ENDING || current.action != LegAction.ANSWERING) {
                runCatching { voiceClient.hangup(token) }
                return
            }
            current.pendingAnswer = false
            current.platformActive = !current.platformInactive
            current.phase = if (current.platformInactive) VoiceCallPhase.INACTIVE else VoiceCallPhase.ACTIVE
            if (telecomGateway == null && !current.platformInactive) legacyAudio.setActive(true)
            voice = voice.copy(state = "active", detail = "Call in progress")
            publish()
        } catch (error: Throwable) {
            if (leg !== current || current.phase == VoiceCallPhase.ENDING) throw error
            if (fromPlatform) throw error
            runCatching { voiceClient.hangup(token) }
            markTerminal(current, error.message ?: "Could not answer the call", failed = true)
            if (telecomGateway == null) {
                clearLeg(token, current.terminalDetail.orEmpty(), failed = true)
            } else {
                disconnectPlatformAndAwait(current)
            }
            throw error
        }
    }

    private suspend fun performLeave(current: Leg, rejected: Boolean, fromPlatform: Boolean) {
        val token = current.token
        if (leg !== current || current.action != LegAction.LEAVING) return
        markTerminal(
            current,
            if (rejected) "Call rejected" else "You left the call",
            failed = false,
            rejected = rejected && current.direction == VoiceCallDirection.INCOMING,
        )
        var failure: Throwable? = null
        try {
            if (rejected && current.direction == VoiceCallDirection.INCOMING) voiceClient.reject(token) else voiceClient.hangup(token)
        } catch (error: Throwable) {
            failure = error
        }
        if (!fromPlatform) {
            try {
                if (telecomGateway == null) {
                    clearLeg(token, current.terminalDetail.orEmpty(), failed = failure != null)
                } else {
                    disconnectPlatformAndAwait(current, rejected)
                }
            } catch (error: Throwable) {
                failure?.addSuppressed(error) ?: run { failure = error }
            }
        } else {
            current.platformDisconnecting = true
            if (telecomGateway == null) {
                clearLeg(token, current.terminalDetail.orEmpty(), failed = failure != null)
            }
        }
        if (failure != null && leg?.token == token) {
            current.terminalFailed = true
            publishNotice(failure.message ?: "The call could not be closed cleanly")
        }
        failure?.let { throw it }
    }

    private suspend fun runUserAction(action: String, token: String, block: suspend () -> Unit) {
        runCatching { block() }.onFailure { error ->
            if (error is CancellationException) throw error
            onDiagnostic(
                "telecom.user-action-failed",
                mapOf("action" to action, "leg_token" to token, "error" to error.toString()),
            )
            val live = leg?.takeIf { it.token == token }
            if (live != null && live.phase != VoiceCallPhase.ENDING) {
                publishNotice(error.message ?: "The call action failed")
            }
        }
    }

    private fun claimAction(current: Leg, requested: LegAction, source: String): Boolean {
        if (leg !== current) return false
        // A local request to leave is allowed to supersede an answer that is
        // still suspended in SIP or Telecom. The answer path observes the
        // changed action after every suspension and rolls back a late accept.
        if (requested == LegAction.LEAVING && current.action == LegAction.ANSWERING) {
            onDiagnostic(
                "telecom.action-superseded",
                mapOf("action" to source, "leg_token" to current.token, "superseded" to "answering"),
            )
            current.action = LegAction.LEAVING
            return true
        }
        if (current.action != LegAction.NONE) {
            onDiagnostic(
                "telecom.duplicate-action",
                mapOf(
                    "action" to source,
                    "leg_token" to current.token,
                    "in_flight" to current.action.name.lowercase(),
                ),
            )
            return false
        }
        current.action = requested
        return true
    }

    private fun releaseAction(current: Leg, expected: LegAction) {
        if (leg === current && current.action == expected) current.action = LegAction.NONE
    }

    private fun activateFromSip(event: VoiceEngineEvent) {
        val current = leg ?: return
        if (current.phase == VoiceCallPhase.ENDING) return
        current.phase = if (current.platformInactive) VoiceCallPhase.INACTIVE else VoiceCallPhase.ACTIVE
        if (telecomGateway == null && !current.platformInactive) legacyAudio.setActive(true)
        voice = voice.copy(state = "active", peer = event.peer.ifBlank { current.peer }, detail = "Call in progress")
        publish()
        if (current.control != null && !current.platformActive && !current.platformActivating && !current.platformInactive) {
            current.platformActivating = true
            scope.launch {
                runCatching { current.control?.setActive() }
                    .onSuccess {
                        current.platformActivating = false
                        if (leg !== current || current.phase == VoiceCallPhase.ENDING) return@onSuccess
                        if (!current.platformInactive) current.platformActive = true
                        publish()
                    }
                    .onFailure {
                        current.platformActivating = false
                        if (leg !== current || current.phase == VoiceCallPhase.ENDING) return@onFailure
                        runCatching { voiceClient.hangup(current.token) }
                        terminateAfterEngineFailure(current, it.message ?: "Android could not activate the call")
                    }
            }
        }
    }

    private fun updateLegPhase(event: VoiceEngineEvent, phase: VoiceCallPhase, detail: String) {
        val current = leg ?: return
        if (current.phase == VoiceCallPhase.ENDING) return
        current.phase = phase
        updateVoice(event, detail)
    }

    private fun updateVoice(event: VoiceEngineEvent, detail: String) {
        val current = leg ?: return
        if (current.phase == VoiceCallPhase.ENDING) return
        voice = voice.copy(state = event.type, peer = event.peer.ifBlank { current.peer }, detail = detail)
        publish()
    }

    private fun finishFromSip(event: VoiceEngineEvent) {
        val current = leg ?: return
        val token = current.token
        scope.launch {
            markTerminal(current, if (event.type == "failed") event.cause.ifBlank { "The call failed" } else "Call ended", event.type == "failed")
            if (telecomGateway == null) {
                clearLeg(token, current.terminalDetail.orEmpty(), failed = current.terminalFailed)
            } else if (!current.platformDisconnecting) {
                runCatching { disconnectPlatformAndAwait(current) }
                    .onFailure {
                        current.terminalFailed = true
                        publishNotice(it.message ?: "Android could not close the call")
                    }
            }
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

    private fun isCurrentLegacyFocus(current: Leg, generation: Long): Boolean =
        leg === current &&
            current.phase != VoiceCallPhase.ENDING &&
            current.legacyFocusGeneration == generation

    private fun schedulePlatformCleanup(token: String, rejected: Boolean, detail: String) {
        val current = leg?.takeIf { it.token == token } ?: return
        if (current.phase != VoiceCallPhase.ENDING) markTerminal(current, detail, failed = true, rejected = rejected)
        scope.launch {
            runCatching {
                if (rejected && current.direction == VoiceCallDirection.INCOMING) voiceClient.reject(token)
                else voiceClient.hangup(token)
            }.onFailure {
                onDiagnostic("telecom.callback-cleanup-engine-failed", mapOf("leg_token" to token, "error" to it.toString()))
            }
            if (leg !== current) return@launch
            runCatching { disconnectPlatformAndAwait(current, rejected) }
                .onFailure {
                    current.terminalFailed = true
                    publishNotice(it.message ?: "Android could not close the call")
                }
        }
    }

    private suspend fun terminateAfterEngineFailure(current: Leg, detail: String) {
        markTerminal(current, detail, failed = true)
        if (telecomGateway == null) {
            clearLeg(current.token, detail, failed = true)
            return
        }
        runCatching { disconnectPlatformAndAwait(current) }
            .onFailure {
                current.terminalFailed = true
                publishNotice(it.message ?: "Android could not close the failed call")
            }
    }

    private suspend fun disconnectPlatformAndAwait(current: Leg, rejected: Boolean = false) {
        current.platformDisconnecting = true
        val job = current.platformJob
        val control = current.control
        try {
            if (control != null) {
                control.disconnect(rejected)
                job?.join()
            } else {
                job?.cancelAndJoin()
            }
        } catch (error: Throwable) {
            current.platformDisconnecting = false
            throw error
        }
    }

    private fun markTerminal(current: Leg, detail: String, failed: Boolean, rejected: Boolean = false) {
        current.phase = VoiceCallPhase.ENDING
        current.terminalDetail = detail
        current.terminalFailed = current.terminalFailed || failed
        current.terminalRejected = current.terminalRejected || rejected
        voice = voice.copy(state = "ending", peer = current.peer, detail = detail)
        publish()
    }

    private fun clearLeg(
        token: String,
        detail: String,
        failed: Boolean = false,
        platformScopeFinished: Boolean = false,
    ) {
        val current = leg?.takeIf { it.token == token } ?: return
        check(telecomGateway == null || platformScopeFinished) {
            "Cannot forget a call before its Android Telecom scope has ended"
        }
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

    private fun publish() {
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
            microphonePermissionRequired = current?.direction == VoiceCallDirection.INCOMING &&
                !voiceClient.hasMicrophonePermission(),
            endingRejected = current?.terminalRejected == true,
            audio = current?.audio ?: PlatformAudioState(),
        )
        _snapshot.value = value
        onSnapshot(value)
    }

    private fun publishNotice(detail: String) {
        voice = voice.copy(detail = detail)
        publish()
    }

    private companion object {
        const val PLATFORM_CALLBACK_TIMEOUT_MS = 4_500L
        val NON_TERMINAL_CALL_EVENTS = setOf("calling", "ringing", "active", "media", "quality", "mute-changed", "dtmf", "dtmf-failed")
    }
}
