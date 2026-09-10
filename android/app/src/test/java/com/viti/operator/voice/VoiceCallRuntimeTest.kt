package com.viti.operator.voice

import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.async
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class VoiceCallRuntimeTest {
    @Test
    fun outgoingSipDialWaitsForTelecomAdmission() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway(deferAdmission = true)
        val legacy = FakeLegacyAudio()
        val runtime = runtime(engine, gateway, legacy)

        runtime.placeCall("+34600000000")
        runCurrent()
        assertTrue(engine.commands.isEmpty())

        gateway.admit()
        runCurrent()
        assertEquals(listOf("call:+34600000000"), engine.commands)
        assertEquals(0, legacy.beginCount)
        assertTrue(runtime.snapshot.value.telecomManaged)
        runtime.close()
    }

    @Test
    fun telecomPolicyRejectionDoesNotBypassSystemWithLegacyFallback() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway(failure = IllegalStateException("native call conflict"))
        val legacy = FakeLegacyAudio()
        val runtime = runtime(engine, gateway, legacy)

        runtime.placeCall("+34600000000")
        runCurrent()

        assertTrue(engine.commands.isEmpty())
        assertEquals(0, legacy.beginCount)
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertTrue(runtime.snapshot.value.voice.detail.contains("native call conflict"))
        runtime.close()
    }

    @Test
    fun staleNotificationTokenCannotEndReplacementCall() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val runtime = runtime(engine, FakeGateway(), FakeLegacyAudio())

        runtime.placeCall("+34600000001")
        runCurrent()
        val first = runtime.snapshot.value.legToken
        runtime.onVoiceEvent(VoiceEngineEvent("failed", legToken = first, cause = "busy"))
        runCurrent()

        runtime.placeCall("+34600000002")
        runCurrent()
        val second = runtime.snapshot.value.legToken
        runtime.hangup(first)
        runCurrent()

        assertFalse(first == second)
        assertEquals(second, runtime.snapshot.value.legToken)
        assertFalse(engine.commands.contains("hangup:$second"))
        runtime.close()
    }

    @Test
    fun telecomCallbacksControlTheExactIncomingLegAndSystemSuppression() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "incoming-generation-7"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000003", legToken = token))
        runCurrent()
        gateway.callbacks.onAnswer()
        gateway.callbacks.onSetInactive()
        runCurrent()

        assertTrue(engine.commands.contains("answer:$token"))
        assertTrue(engine.commands.contains("inactive:$token:true"))
        assertEquals(VoiceCallPhase.INACTIVE, runtime.snapshot.value.phase)
        runtime.close()
    }

    @Test
    fun legacyFallbackIsUsedOnlyWhenCoreTelecomIsUnavailable() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val legacy = FakeLegacyAudio()
        val runtime = runtime(engine, null, legacy)

        runtime.placeCall("+34600000004")
        runCurrent()

        assertEquals(1, legacy.beginCount)
        assertEquals(listOf("call:+34600000004"), engine.commands)
        assertFalse(runtime.snapshot.value.telecomManaged)
        assertTrue(runtime.snapshot.value.platformReady)
        runtime.close()
    }

    @Test
    fun provisioningChangeIsDeferredUntilTheCurrentLegEnds() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val runtime = runtime(engine, FakeGateway(), FakeLegacyAudio())
        val next = VoiceProvisioning(enabled = false)

        runtime.placeCall("+34600000005")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.start(next)
        assertTrue(engine.starts.isEmpty())

        runtime.onVoiceEvent(VoiceEngineEvent("ended", legToken = token))
        runCurrent()
        assertEquals(listOf(next), engine.starts)
        runtime.close()
    }

    @Test
    fun unexpectedTelecomScopeEndClearsTheMatchingSipLeg() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())

        runtime.placeCall("+34600000006")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        gateway.finish()
        runCurrent()

        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertTrue(engine.commands.contains("hangup:$token"))
        assertTrue(runtime.snapshot.value.voice.detail.contains("Android ended its call scope"))
        runtime.close()
    }

    @Test
    fun sipSetupFailureRollsBackTheAdmittedTelecomCall() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine().apply { callFailure = IllegalStateException("SIP unavailable") }
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())

        runtime.placeCall("+34600000007")
        runCurrent()

        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertEquals(1, gateway.disconnectCount)
        assertTrue(runtime.snapshot.value.voice.detail.contains("SIP unavailable"))
        runtime.close()
    }

    @Test
    fun failedPlatformDisconnectKeepsTheLegUntilTelecomReallyEnds() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway().apply { disconnectFailure = IllegalStateException("Telecom still owns it") }
        val runtime = runtime(engine, gateway, FakeLegacyAudio())

        runtime.placeCall("+34600000008")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.hangup(token)
        runCurrent()

        assertEquals(token, runtime.snapshot.value.legToken)
        assertEquals(VoiceCallPhase.ENDING, runtime.snapshot.value.phase)
        assertTrue(gateway.scopeActive)

        gateway.disconnectFailure = null
        runtime.hangup(token)
        runCurrent()
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertFalse(gateway.scopeActive)
        runtime.close()
    }

    @Test
    fun successfulDisconnectStillWaitsForTheTelecomScopeToFinish() = runTest(UnconfinedTestDispatcher()) {
        val gateway = FakeGateway(completeOnDisconnect = false)
        val runtime = runtime(FakeVoiceEngine(), gateway, FakeLegacyAudio())

        runtime.placeCall("+34600000009")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.hangup(token)
        runCurrent()

        assertEquals(token, runtime.snapshot.value.legToken)
        assertEquals(VoiceCallPhase.ENDING, runtime.snapshot.value.phase)
        gateway.finish()
        runCurrent()
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        runtime.close()
    }

    @Test
    fun incomingCallWithoutMicrophonePermissionIsStillAdmittedOrRejectedByTelecom() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine().apply { microphonePermission = false }
        val gateway = FakeGateway(failure = IllegalStateException("native call conflict"))
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "incoming-without-microphone"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000010", legToken = token))
        runCurrent()

        assertTrue(engine.commands.contains("reject:$token"))
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertTrue(runtime.snapshot.value.voice.detail.contains("native call conflict"))
        runtime.close()
    }

    @Test
    fun telecomAnswerWithoutMicrophonePermissionRejectsTheExactIncomingLeg() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine().apply { microphonePermission = false }
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "incoming-system-answer-without-microphone"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000011", legToken = token))
        runCurrent()
        assertTrue(runtime.snapshot.value.telecomManaged)
        assertTrue(runtime.snapshot.value.microphonePermissionRequired)
        val answerResult = runCatching { gateway.callbacks.onAnswer() }
        runCurrent()

        assertTrue(answerResult.isFailure)
        assertTrue(engine.commands.contains("reject:$token"))
        assertEquals(1, gateway.disconnectCount)
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        runtime.close()
    }

    @Test
    fun lateSipActiveEventCannotUndoTelecomInactiveState() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "incoming-inactive-generation"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000012", legToken = token))
        runCurrent()
        gateway.callbacks.onAnswer()
        gateway.callbacks.onSetInactive()
        runtime.onVoiceEvent(VoiceEngineEvent("active", peer = "+34600000012", legToken = token))
        runCurrent()

        assertEquals(VoiceCallPhase.INACTIVE, runtime.snapshot.value.phase)
        assertEquals(0, gateway.setActiveCount)
        assertEquals("inactive:$token:true", engine.commands.last())
        runtime.close()
    }

    @Test
    fun failedSipActivationClosesTelecomBeforeForgettingTheLeg() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway().apply { setActiveFailure = IllegalStateException("activation denied") }
        val runtime = runtime(engine, gateway, FakeLegacyAudio())

        runtime.placeCall("+34600000013")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.onVoiceEvent(VoiceEngineEvent("active", peer = "+34600000013", legToken = token))
        runCurrent()

        assertEquals(1, gateway.setActiveCount)
        assertEquals(1, gateway.disconnectCount)
        assertTrue(engine.commands.contains("hangup:$token"))
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertFalse(gateway.scopeActive)
        runtime.close()
    }

    @Test
    fun endingLegRejectsLateSipEventsCallbacksAndAnswerActions() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway().apply { disconnectFailure = IllegalStateException("still owned") }
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "retained-ending-incoming"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000014", legToken = token))
        runCurrent()
        runtime.reject(token)
        runCurrent()
        assertEquals(VoiceCallPhase.ENDING, runtime.snapshot.value.phase)
        assertEquals("ending", runtime.snapshot.value.voice.state)
        assertTrue(runtime.snapshot.value.endingRejected)

        runtime.answer(token)
        runtime.onVoiceEvent(VoiceEngineEvent("ringing", legToken = token))
        runtime.onVoiceEvent(VoiceEngineEvent("active", legToken = token))
        gateway.callbacks.onSetActive()
        gateway.callbacks.onSetInactive()
        runCurrent()

        assertEquals(VoiceCallPhase.ENDING, runtime.snapshot.value.phase)
        assertEquals("ending", runtime.snapshot.value.voice.state)
        assertFalse(engine.commands.any { it == "answer:$token" })
        assertFalse(engine.commands.any { it == "inactive:$token:false" || it == "inactive:$token:true" })
        assertEquals(0, gateway.setActiveCount)
        runtime.close()
    }

    @Test
    fun legacyAudioFocusLossBlocksLateSipReactivation() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val legacy = FakeLegacyAudio()
        val runtime = runtime(engine, null, legacy)

        runtime.placeCall("+34600000019")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.onLegacyInactiveChanged(true)
        runtime.onVoiceEvent(VoiceEngineEvent("active", peer = "+34600000019", legToken = token))
        runCurrent()

        assertEquals(VoiceCallPhase.INACTIVE, runtime.snapshot.value.phase)
        assertTrue(engine.commands.contains("inactive:$token:true"))
        assertTrue(legacy.activeRequests.isEmpty())
        runtime.close()
    }

    @Test
    fun legacyAnswerCompletionCannotReclaimAudioAfterFocusLoss() = runTest(UnconfinedTestDispatcher()) {
        val gate = CompletableDeferred<Unit>()
        val engine = FakeVoiceEngine().apply { answerGate = gate }
        val legacy = FakeLegacyAudio()
        val runtime = runtime(engine, null, legacy)
        val token = "legacy-answer-inactive-interleaving"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000020", legToken = token))
        runCurrent()
        runtime.answer(token)
        runCurrent()
        runtime.onLegacyInactiveChanged(true)
        gate.complete(Unit)
        runCurrent()

        assertEquals(VoiceCallPhase.INACTIVE, runtime.snapshot.value.phase)
        assertTrue(legacy.activeRequests.isEmpty())
        runtime.close()
    }

    @Test
    fun failedLegacyInactiveSuppressionEndsTheLocalCall() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine().apply { inactiveFailure = IllegalStateException("media suppression failed") }
        val legacy = FakeLegacyAudio()
        val runtime = runtime(engine, null, legacy)

        runtime.placeCall("+34600000021")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.onLegacyInactiveChanged(true)
        runCurrent()

        assertTrue(engine.commands.contains("hangup:$token"))
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertEquals(1, legacy.endCount)
        runtime.close()
    }

    @Test
    fun lateLegacyFocusLossSuccessCannotOverwriteACompleteFocusGain() = runTest(UnconfinedTestDispatcher()) {
        val lossGate = CompletableDeferred<Unit>()
        val engine = FakeVoiceEngine().apply { inactiveTrueGate = lossGate }
        val runtime = runtime(engine, null, FakeLegacyAudio())

        runtime.placeCall("+34600000022")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.onLegacyInactiveChanged(true)
        runCurrent()
        runtime.onLegacyInactiveChanged(false)
        runCurrent()
        lossGate.complete(Unit)
        runCurrent()

        assertEquals(listOf("inactive:$token:true", "inactive:$token:false"), engine.commands.filter { it.startsWith("inactive:") })
        assertEquals(VoiceCallPhase.ACTIVE, runtime.snapshot.value.phase)
        assertEquals(token, runtime.snapshot.value.legToken)
        runtime.close()
    }

    @Test
    fun lateLegacyFocusLossFailureCannotEndTheCallAfterFocusReturns() = runTest(UnconfinedTestDispatcher()) {
        val lossGate = CompletableDeferred<Unit>()
        val engine = FakeVoiceEngine().apply {
            inactiveTrueGate = lossGate
            inactiveFailure = IllegalStateException("obsolete suppression failure")
        }
        val runtime = runtime(engine, null, FakeLegacyAudio())

        runtime.placeCall("+34600000023")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.onLegacyInactiveChanged(true)
        runCurrent()
        runtime.onLegacyInactiveChanged(false)
        runCurrent()
        lossGate.complete(Unit)
        runCurrent()

        assertFalse(engine.commands.contains("hangup:$token"))
        assertEquals(listOf("inactive:$token:false"), engine.commands.filter { it.startsWith("inactive:") })
        assertEquals(VoiceCallPhase.ACTIVE, runtime.snapshot.value.phase)
        assertEquals(token, runtime.snapshot.value.legToken)
        runtime.close()
    }

    @Test
    fun answerCompletionCannotUndoInactiveCallbackThatArrivedWhileItWasPending() = runTest(UnconfinedTestDispatcher()) {
        val gate = CompletableDeferred<Unit>()
        val engine = FakeVoiceEngine().apply { answerGate = gate }
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "answer-inactive-interleaving"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000015", legToken = token))
        runCurrent()
        val answering = launch { gateway.callbacks.onAnswer() }
        runCurrent()
        gateway.callbacks.onSetInactive()
        gate.complete(Unit)
        answering.join()
        runCurrent()

        assertEquals(VoiceCallPhase.INACTIVE, runtime.snapshot.value.phase)
        assertTrue(engine.commands.contains("inactive:$token:true"))
        runtime.close()
    }

    @Test
    fun platformAnswerTimeoutReturnsWithinBudgetAndCleansUpAsynchronously() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine().apply { answerGate = CompletableDeferred() }
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "answer-timeout"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000016", legToken = token))
        runCurrent()
        val result = async { runCatching { gateway.callbacks.onAnswer() } }
        advanceTimeBy(4_501)
        runCurrent()

        assertTrue(result.await().isFailure)
        assertTrue(engine.commands.contains("hangup:$token"))
        assertEquals(1, gateway.disconnectCount)
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        runtime.close()
    }

    @Test
    fun remoteSurfaceDisconnectLeavesOnlyThisLocalParticipant() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "car-remote-disconnect"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000024", legToken = token))
        runCurrent()
        gateway.callbacks.onAnswer()
        gateway.callbacks.onDisconnect(rejected = false)

        assertTrue(engine.commands.contains("hangup:$token"))
        assertEquals(0, gateway.disconnectCount)
        assertEquals(VoiceCallPhase.ENDING, runtime.snapshot.value.phase)

        gateway.finish()
        runCurrent()
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        runtime.close()
    }

    @Test
    fun remoteSurfaceRejectsTheExactIncomingParticipant() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine()
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "car-remote-reject"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000026", legToken = token))
        runCurrent()
        gateway.callbacks.onDisconnect(rejected = true)

        assertTrue(engine.commands.contains("reject:$token"))
        assertFalse(engine.commands.contains("hangup:$token"))
        assertEquals(VoiceCallPhase.ENDING, runtime.snapshot.value.phase)
        runtime.close()
    }

    @Test
    fun telecomAudioTracksBluetoothAndUsbChangesAndRoutesRequestsThroughAndroid() = runTest(UnconfinedTestDispatcher()) {
        val gateway = FakeGateway()
        val runtime = runtime(FakeVoiceEngine(), gateway, FakeLegacyAudio())
        runtime.placeCall("+34600000027")
        runCurrent()
        val bluetooth = VoiceAudioEndpoint("bt", "Car Bluetooth", VoiceAudioEndpointType.BLUETOOTH)
        val usb = VoiceAudioEndpoint("usb", "USB headset", VoiceAudioEndpointType.WIRED_HEADSET)

        gateway.callbacks.onAudioState(PlatformAudioState(current = bluetooth, available = listOf(bluetooth, usb)))
        assertEquals(bluetooth, runtime.snapshot.value.audio.current)
        runtime.requestEndpoint(usb.id)
        runCurrent()
        assertEquals(listOf("usb"), gateway.endpointRequests)
        gateway.callbacks.onAudioState(PlatformAudioState(current = usb, available = listOf(usb)))

        assertEquals(usb, runtime.snapshot.value.audio.current)
        assertEquals(listOf(usb), runtime.snapshot.value.audio.available)
        runtime.close()
    }

    @Test
    fun remoteSurfaceDisconnectTimesOutBeforeThePlatformDeadline() = runTest(UnconfinedTestDispatcher()) {
        val hangGate = CompletableDeferred<Unit>()
        val engine = FakeVoiceEngine().apply { this.hangGate = hangGate }
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "car-disconnect-timeout"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000025", legToken = token))
        runCurrent()
        gateway.callbacks.onAnswer()
        val result = async { runCatching { gateway.callbacks.onDisconnect(rejected = false) } }
        advanceTimeBy(4_501)
        runCurrent()

        assertTrue(result.await().isFailure)
        assertEquals(VoiceCallPhase.ENDING, runtime.snapshot.value.phase)
        gateway.finish()
        runCurrent()
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        runtime.close()
    }

    @Test
    fun lateDialFailureCannotMutateStateAfterTheLegWasRemoved() = runTest(UnconfinedTestDispatcher()) {
        val gate = CompletableDeferred<Unit>()
        val engine = FakeVoiceEngine().apply {
            callGate = gate
            callFailure = IllegalStateException("late dial failure")
        }
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())

        runtime.placeCall("+34600000017")
        runCurrent()
        val token = runtime.snapshot.value.legToken
        runtime.hangup(token)
        runCurrent()
        assertTrue(runtime.snapshot.value.legToken.isBlank())

        gate.complete(Unit)
        runCurrent()

        assertEquals("registered", runtime.snapshot.value.voice.state)
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertEquals(1, gateway.disconnectCount)
        runtime.close()
    }

    @Test
    fun lateUiAnswerFailureCannotMutateStateAfterTheLegWasRemoved() = runTest(UnconfinedTestDispatcher()) {
        val gate = CompletableDeferred<Unit>()
        val engine = FakeVoiceEngine().apply {
            answerGate = gate
            answerFailure = IllegalStateException("late answer failure")
        }
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "late-ui-answer"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000018", legToken = token))
        runCurrent()
        runtime.answer(token)
        runCurrent()
        runtime.hangup(token)
        runCurrent()
        assertTrue(runtime.snapshot.value.legToken.isBlank())

        gate.complete(Unit)
        runCurrent()

        assertEquals("registered", runtime.snapshot.value.voice.state)
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        assertEquals(1, gateway.disconnectCount)
        runtime.close()
    }

    @Test
    fun failedInactiveSuppressionEndsTheCallInsteadOfClaimingPrivacy() = runTest(UnconfinedTestDispatcher()) {
        val engine = FakeVoiceEngine().apply { inactiveFailure = IllegalStateException("media suppression failed") }
        val gateway = FakeGateway()
        val runtime = runtime(engine, gateway, FakeLegacyAudio())
        val token = "inactive-suppression-failure"

        runtime.onVoiceEvent(VoiceEngineEvent("incoming", peer = "+34600000017", legToken = token))
        runCurrent()
        gateway.callbacks.onAnswer()
        val result = runCatching { gateway.callbacks.onSetInactive() }
        runCurrent()

        assertTrue(result.isFailure)
        assertTrue(engine.commands.contains("hangup:$token"))
        assertTrue(runtime.snapshot.value.legToken.isBlank())
        runtime.close()
    }

    private fun kotlinx.coroutines.test.TestScope.runtime(
        engine: FakeVoiceEngine,
        gateway: PlatformCallGateway?,
        legacy: FakeLegacyAudio,
    ) = VoiceCallRuntime(
        scope = this,
        voiceClient = engine,
        telecomGateway = gateway,
        legacyAudio = legacy,
        onSnapshot = {},
    )

    private class FakeVoiceEngine : VoiceEngineControl {
        val commands = mutableListOf<String>()
        val starts = mutableListOf<VoiceProvisioning>()
        var microphonePermission = true
        var callFailure: Throwable? = null
        var callGate: CompletableDeferred<Unit>? = null
        var answerGate: CompletableDeferred<Unit>? = null
        var hangGate: CompletableDeferred<Unit>? = null
        var answerFailure: Throwable? = null
        var inactiveFailure: Throwable? = null
        var inactiveTrueGate: CompletableDeferred<Unit>? = null

        override fun hasMicrophonePermission() = microphonePermission
        override fun start(provisioning: VoiceProvisioning) { starts += provisioning }
        override suspend fun call(legToken: String, number: String, traceId: String) {
            callGate?.await()
            callFailure?.let { throw it }
            commands += "call:$number"
        }
        override suspend fun answer(legToken: String) {
            answerGate?.await()
            answerFailure?.let { throw it }
            commands += "answer:$legToken"
        }
        override suspend fun reject(legToken: String) { commands += "reject:$legToken" }
        override suspend fun hangup(legToken: String) {
            hangGate?.await()
            commands += "hangup:$legToken"
        }
        override suspend fun setMuted(legToken: String, muted: Boolean) { commands += "mute:$legToken:$muted" }
        override suspend fun setTelecomMuted(legToken: String, muted: Boolean) { commands += "telecom-mute:$legToken:$muted" }
        override suspend fun setSystemInactive(legToken: String, inactive: Boolean) {
            if (inactive) inactiveTrueGate?.await()
            inactiveFailure?.takeIf { inactive }?.let { throw it }
            commands += "inactive:$legToken:$inactive"
        }
        override suspend fun sendDtmf(legToken: String, tone: Char) { commands += "dtmf:$legToken:$tone" }
        override fun destroy() = Unit
    }

    private class FakeGateway(
        private val deferAdmission: Boolean = false,
        private val failure: Throwable? = null,
        private val completeOnDisconnect: Boolean = true,
    ) : PlatformCallGateway {
        var disconnectCount = 0
        var setActiveCount = 0
        var disconnectFailure: Throwable? = null
        var setActiveFailure: Throwable? = null
        var scopeActive = false
        val endpointRequests = mutableListOf<String>()
        lateinit var callbacks: PlatformCallCallbacks
        private var ready: ((PlatformCallControl) -> Unit)? = null
        private val admission = CompletableDeferred<Unit>()
        private var lifetime = CompletableDeferred<Unit>()
        private val control = object : PlatformCallControl {
            override suspend fun answer() = Unit
            override suspend fun setActive() {
                setActiveCount += 1
                setActiveFailure?.let { throw it }
            }
            override suspend fun setInactive() = Unit
            override suspend fun disconnect(rejected: Boolean) {
                disconnectCount += 1
                disconnectFailure?.let { throw it }
                if (completeOnDisconnect) lifetime.complete(Unit)
            }
            override suspend fun requestEndpoint(endpointId: String) {
                endpointRequests += endpointId
            }
        }

        override suspend fun runCall(
            request: PlatformCallRequest,
            callbacks: PlatformCallCallbacks,
            onReady: (PlatformCallControl) -> Unit,
        ) {
            scopeActive = true
            try {
                failure?.let { throw it }
                lifetime = CompletableDeferred()
                this.callbacks = callbacks
                if (deferAdmission) {
                    ready = onReady
                    admission.await()
                } else {
                    onReady(control)
                }
                lifetime.await()
            } finally {
                scopeActive = false
            }
        }

        fun admit() {
            ready?.invoke(control)
            admission.complete(Unit)
        }

        fun finish() {
            lifetime.complete(Unit)
        }
    }

    private class FakeLegacyAudio : LegacyAudioControl {
        var beginCount = 0
        var endCount = 0
        val activeRequests = mutableListOf<Boolean>()
        override fun begin(direction: VoiceCallDirection) { beginCount += 1 }
        override fun setActive(value: Boolean) { activeRequests += value }
        override fun requestEndpoint(endpointId: String) = Unit
        override fun end() { endCount += 1 }
        override fun close() = Unit
    }
}
