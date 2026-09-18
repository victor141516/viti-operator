package com.viti.operator.voice

import com.viti.operator.data.ServerSettings
import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class VoiceProvisioningSupervisorTest {
    @Test
    fun reportsNativeServerReachabilityWhileTheUiIsAbsent() = runTest(UnconfinedTestDispatcher()) {
        val settings = MutableStateFlow(ServerSettings("http://server", "key"))
        val reachability = mutableListOf<Boolean>()
        var attempts = 0

        val job = launchVoiceProvisioningSupervisor(
            settings = settings,
            load = {
                attempts += 1
                if (attempts == 1) error("offline")
                VoiceProvisioning(enabled = true, websocketUrl = "ws://voice")
            },
            apply = {},
            onReachabilityChanged = reachability::add,
            retryDelayMs = 5_000,
            refreshDelayMs = 10_000,
        )
        runCurrent()
        assertEquals(listOf(false), reachability)

        advanceTimeBy(5_000)
        runCurrent()
        assertEquals(listOf(false, true), reachability)

        advanceTimeBy(10_000)
        runCurrent()
        assertEquals(listOf(false, true, true), reachability)
        job.cancel()
    }

    @Test
    fun retriesAfterFailureWithoutNeedingAnActivityOrCarSession() = runTest(UnconfinedTestDispatcher()) {
        val settings = MutableStateFlow(ServerSettings("http://first", "key"))
        val applied = mutableListOf<VoiceProvisioning>()
        val failures = mutableListOf<String>()
        var attempts = 0
        val expected = VoiceProvisioning(enabled = true, websocketUrl = "ws://voice")

        val job = launchVoiceProvisioningSupervisor(
            settings = settings,
            load = {
                attempts += 1
                if (attempts == 1) error("offline")
                expected
            },
            apply = applied::add,
            onFailure = { failures += it.message.orEmpty() },
            retryDelayMs = 5_000,
            refreshDelayMs = 30_000,
        )
        runCurrent()
        assertEquals(listOf("offline"), failures)
        assertEquals(emptyList<VoiceProvisioning>(), applied)

        advanceTimeBy(5_000)
        runCurrent()

        assertEquals(listOf(expected), applied)
        job.cancel()
    }

    @Test
    fun settingsChangeCancelsAnObsoleteProvisioningResponse() = runTest(UnconfinedTestDispatcher()) {
        val oldSettings = ServerSettings("http://old", "old-key")
        val newSettings = ServerSettings("http://new", "new-key")
        val settings = MutableStateFlow(oldSettings)
        val oldGate = CompletableDeferred<Unit>()
        val applied = mutableListOf<String>()

        val job = launchVoiceProvisioningSupervisor(
            settings = settings,
            load = { current ->
                if (current == oldSettings) oldGate.await()
                VoiceProvisioning(enabled = true, websocketUrl = current.serverUrl)
            },
            apply = { applied += it.websocketUrl },
        )
        runCurrent()
        settings.value = newSettings
        runCurrent()
        oldGate.complete(Unit)
        runCurrent()

        assertEquals(listOf("http://new"), applied)
        job.cancel()
    }

    @Test
    fun unchangedRefreshDoesNotRestartTheSipEngine() = runTest(UnconfinedTestDispatcher()) {
        val settings = MutableStateFlow(ServerSettings("http://server", "key"))
        val expected = VoiceProvisioning(enabled = true, websocketUrl = "ws://voice")
        val applied = mutableListOf<VoiceProvisioning>()

        val job = launchVoiceProvisioningSupervisor(
            settings = settings,
            load = { expected },
            apply = applied::add,
            refreshDelayMs = 30_000,
        )
        runCurrent()
        advanceTimeBy(90_000)
        runCurrent()

        assertEquals(listOf(expected), applied)
        job.cancel()
    }
}
