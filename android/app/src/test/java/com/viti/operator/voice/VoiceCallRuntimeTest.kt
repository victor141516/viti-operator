package com.viti.operator.voice

import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
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

        override fun hasMicrophonePermission() = microphonePermission
        override fun start(provisioning: VoiceProvisioning) { starts += provisioning }
        override suspend fun call(legToken: String, number: String, traceId: String) {
            callFailure?.let { throw it }
            commands += "call:$number"
        }
        override suspend fun answer(legToken: String) { commands += "answer:$legToken" }
        override suspend fun reject(legToken: String) { commands += "reject:$legToken" }
        override suspend fun hangup(legToken: String) { commands += "hangup:$legToken" }
        override suspend fun setMuted(legToken: String, muted: Boolean) { commands += "mute:$legToken:$muted" }
        override suspend fun setTelecomMuted(legToken: String, muted: Boolean) { commands += "telecom-mute:$legToken:$muted" }
        override suspend fun setSystemInactive(legToken: String, inactive: Boolean) { commands += "inactive:$legToken:$inactive" }
        override suspend fun sendDtmf(legToken: String, tone: Char) { commands += "dtmf:$legToken:$tone" }
        override fun destroy() = Unit
    }

    private class FakeGateway(
        private val deferAdmission: Boolean = false,
        private val failure: Throwable? = null,
    ) : PlatformCallGateway {
        var disconnectCount = 0
        lateinit var callbacks: PlatformCallCallbacks
        private var ready: ((PlatformCallControl) -> Unit)? = null
        private val admission = CompletableDeferred<Unit>()
        private var lifetime = CompletableDeferred<Unit>()
        private val control = object : PlatformCallControl {
            override suspend fun answer() = Unit
            override suspend fun setActive() = Unit
            override suspend fun setInactive() = Unit
            override suspend fun disconnect(rejected: Boolean) {
                disconnectCount += 1
                lifetime.complete(Unit)
            }
            override suspend fun requestEndpoint(endpointId: String) = Unit
        }

        override suspend fun runCall(
            request: PlatformCallRequest,
            callbacks: PlatformCallCallbacks,
            onReady: (PlatformCallControl) -> Unit,
        ) {
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
        override fun begin(direction: VoiceCallDirection) { beginCount += 1 }
        override fun setActive(value: Boolean) = Unit
        override fun requestEndpoint(endpointId: String) = Unit
        override fun end() = Unit
        override fun close() = Unit
    }
}
