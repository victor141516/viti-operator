package com.viti.operator.voice

import com.viti.operator.data.RuntimeStatus
import com.viti.operator.data.ServerSettings
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import org.junit.Assert.assertEquals
import org.junit.Test

@OptIn(ExperimentalCoroutinesApi::class)
class CarrierStatusSupervisorTest {
    private val healthy = RuntimeStatus(ready = true, tunnelReady = true, imsReady = true)

    @Test
    fun carrierOutageChangesStatusButDoesNotStartLocalRinging() = runTest(UnconfinedTestDispatcher()) {
        val states = MutableStateFlow(CarrierAvailability.UNKNOWN)
        var status = RuntimeStatus(simReady = true)
        var visible = false
        val monitor = launchServerConnectionAlertMonitor(
            states.map { !callAvailability(true, true, it).needsLocalConnectionAlert() },
            showAlert = { visible = true }, dismissAlert = { visible = false },
        )
        val poller = launchCarrierStatusSupervisor(
            MutableStateFlow(ServerSettings()), load = { status }, onState = { states.value = it },
        )
        advanceTimeBy(30_000)
        runCurrent()
        assertEquals(CarrierAvailability.DOWN, states.value)
        assertEquals(CallAvailability.NO_CARRIER, callAvailability(true, true, states.value))
        assertEquals(false, visible)
        advanceTimeBy(120_000)
        runCurrent()
        assertEquals(false, visible)
        // Losing SIM access alone must not block an established carrier path.
        status = healthy.copy(simReady = false)
        advanceTimeBy(10_000)
        runCurrent()
        assertEquals(CarrierAvailability.READY, states.value)
        assertEquals(false, visible)
        poller.cancel()
        monitor.cancel()
    }

    @Test
    fun localConnectionLossStillAlertsEvenDuringCarrierOutage() {
        assertEquals(true, callAvailability(false, true, CarrierAvailability.DOWN).needsLocalConnectionAlert())
        assertEquals(true, callAvailability(true, false, CarrierAvailability.DOWN).needsLocalConnectionAlert())
        assertEquals(true, callAvailability(true, true, CarrierAvailability.UNREACHABLE).needsLocalConnectionAlert())
        assertEquals(false, callAvailability(true, true, CarrierAvailability.DOWN).needsLocalConnectionAlert())
        assertEquals(false, callAvailability(true, true, CarrierAvailability.READY).needsLocalConnectionAlert())
    }

    @Test
    fun readinessRequiresTheTunnelAndImsNotJustAnHttpResponse() = runTest(UnconfinedTestDispatcher()) {
        var response = healthy.copy(tunnelReady = false)
        var current = CarrierAvailability.UNKNOWN
        val job = launchCarrierStatusSupervisor(
            MutableStateFlow(ServerSettings()), load = { response }, onState = { current = it },
        )
        assertEquals(CarrierAvailability.DOWN, current)
        response = healthy.copy(imsReady = false)
        advanceTimeBy(10_000)
        runCurrent()
        assertEquals(CarrierAvailability.DOWN, current)
        response = healthy.copy(ready = false)
        advanceTimeBy(10_000)
        runCurrent()
        assertEquals(CarrierAvailability.DOWN, current)
        job.cancel()
    }

    @Test
    fun failedStatusProbeInvalidatesPreviousSuccessAndKeepsRetrying() = runTest(UnconfinedTestDispatcher()) {
        var fail = false
        var current = CarrierAvailability.UNKNOWN
        val job = launchCarrierStatusSupervisor(
            MutableStateFlow(ServerSettings()),
            load = { if (fail) error("offline") else healthy }, onState = { current = it },
        )
        assertEquals(CarrierAvailability.READY, current)
        fail = true
        advanceTimeBy(10_000)
        runCurrent()
        assertEquals(CarrierAvailability.UNREACHABLE, current)
        fail = false
        advanceTimeBy(10_000)
        runCurrent()
        assertEquals(CarrierAvailability.READY, current)
        job.cancel()
    }

    @Test
    fun hungProbeTimesOutAndCanRecover() = runTest(UnconfinedTestDispatcher()) {
        var hang = true
        var current = CarrierAvailability.UNKNOWN
        val job = launchCarrierStatusSupervisor(
            MutableStateFlow(ServerSettings()),
            load = { if (hang) awaitCancellation() else healthy },
            onState = { current = it }, requestTimeoutMs = 1_000,
        )
        advanceTimeBy(1_000)
        runCurrent()
        assertEquals(CarrierAvailability.UNREACHABLE, current)
        hang = false
        advanceTimeBy(10_000)
        runCurrent()
        assertEquals(CarrierAvailability.READY, current)
        job.cancel()
    }

    @Test
    fun serverChangeClearsOldReadinessAndCancelsOldRequest() = runTest(UnconfinedTestDispatcher()) {
        val settings = MutableStateFlow(ServerSettings("http://first"))
        val pending = CompletableDeferred<RuntimeStatus>()
        val observed = mutableListOf<CarrierAvailability>()
        val job = launchCarrierStatusSupervisor(
            settings,
            load = { if (it.serverUrl == "http://first") pending.await() else healthy },
            onState = observed::add,
        )
        settings.value = ServerSettings("http://second")
        runCurrent()
        pending.complete(RuntimeStatus())
        runCurrent()
        assertEquals(listOf(CarrierAvailability.UNKNOWN, CarrierAvailability.UNKNOWN, CarrierAvailability.READY), observed)
        job.cancel()
        advanceTimeBy(60_000)
        runCurrent()
        assertEquals(3, observed.size)
    }
}
