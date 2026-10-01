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
        assertEquals(CallAvailability.CONNECTING, callAvailability(true, null, CarrierAvailability.UNKNOWN))
        assertEquals(CallAvailability.READY, callAvailability(true, true, CarrierAvailability.READY))
        assertEquals(CallAvailability.NO_SERVER, callAvailability(true, false, CarrierAvailability.READY))
        assertEquals(CallAvailability.NO_VOICE, callAvailability(false, true, CarrierAvailability.READY))
        assertEquals(CallAvailability.NO_CARRIER, callAvailability(true, true, CarrierAvailability.DOWN))
        assertEquals(CallAvailability.NO_SERVER, callAvailability(true, true, CarrierAvailability.UNREACHABLE))
        assertEquals(CallAvailability.CONNECTING, callAvailability(true, true, CarrierAvailability.UNKNOWN))
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

        // The outage does not expire with a SIP alert or a 30-second ringtone.
        advanceTimeBy(120_000)
        runCurrent()
        assertEquals(1, shown)
        assertEquals(1, dismissed)

        available.value = true
        runCurrent()
        assertEquals(2, dismissed)
        job.cancel()
    }
}
