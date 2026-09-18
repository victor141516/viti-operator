package com.viti.operator.voice

import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class ServerConnectionAlertMonitorTest {
    @Test
    fun nativeApiFailureOverridesAStaleRegisteredWebView() {
        assertEquals(true, effectiveServerConnection(voiceRegistered = true, serverApiReachable = null))
        assertEquals(true, effectiveServerConnection(voiceRegistered = true, serverApiReachable = true))
        assertEquals(false, effectiveServerConnection(voiceRegistered = true, serverApiReachable = false))
        assertEquals(false, effectiveServerConnection(voiceRegistered = false, serverApiReachable = true))
    }

    @Test
    fun initialConnectionGetsAStartupGracePeriod() = runTest(UnconfinedTestDispatcher()) {
        val available = MutableStateFlow(false)
        var shown = 0
        val job = launchServerConnectionAlertMonitor(
            connectionAvailable = available,
            showAlert = { shown += 1 },
            dismissAlert = {},
            initialGraceMs = 30_000,
            reconnectGraceMs = 15_000,
        )

        advanceTimeBy(29_999)
        runCurrent()
        assertEquals(0, shown)
        advanceTimeBy(1)
        runCurrent()
        assertEquals(1, shown)
        job.cancel()
    }

    @Test
    fun reconnectBeforeTheGracePeriodPreventsTheAlert() = runTest(UnconfinedTestDispatcher()) {
        val available = MutableStateFlow(true)
        var shown = 0
        var dismissed = 0
        val job = launchServerConnectionAlertMonitor(
            connectionAvailable = available,
            showAlert = { shown += 1 },
            dismissAlert = { dismissed += 1 },
            initialGraceMs = 30_000,
            reconnectGraceMs = 15_000,
        )
        runCurrent()

        available.value = false
        runCurrent()
        advanceTimeBy(10_000)
        available.value = true
        runCurrent()
        advanceTimeBy(10_000)
        runCurrent()

        assertEquals(0, shown)
        assertEquals(2, dismissed)
        job.cancel()
    }

    @Test
    fun establishedConnectionUsesShorterGraceAndRecoveryDismissesTheAlert() = runTest(UnconfinedTestDispatcher()) {
        val available = MutableStateFlow(true)
        var shown = 0
        var dismissed = 0
        val job = launchServerConnectionAlertMonitor(
            connectionAvailable = available,
            showAlert = { shown += 1 },
            dismissAlert = { dismissed += 1 },
            initialGraceMs = 30_000,
            reconnectGraceMs = 15_000,
        )
        runCurrent()

        available.value = false
        runCurrent()
        advanceTimeBy(15_000)
        runCurrent()
        assertEquals(1, shown)

        available.value = true
        runCurrent()
        assertEquals(2, dismissed)
        job.cancel()
    }
}
