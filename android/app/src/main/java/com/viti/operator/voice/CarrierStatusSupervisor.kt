package com.viti.operator.voice

import com.viti.operator.data.RuntimeStatus
import com.viti.operator.data.ServerSettings
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.delay
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeout

internal enum class CarrierAvailability { UNKNOWN, READY, DOWN, UNREACHABLE }

internal enum class CallAvailability(val title: String, val detail: String) {
    CONNECTING("Checking call availability", "Checking the server and carrier connection…"),
    READY("Viti Operator", "Service calls are ready"),
    NO_SERVER("No connection", "Cannot reach Viti Server. Carrier calls cannot be received on this device."),
    NO_VOICE("No connection", "Voice registration is unavailable. Carrier calls cannot be received on this device."),
    NO_CARRIER("Carrier connection down", "Viti Server cannot reach the carrier. Incoming and outgoing carrier calls are unavailable."),
}

internal fun callAvailability(
    voiceRegistered: Boolean,
    serverApiReachable: Boolean?,
    carrier: CarrierAvailability,
): CallAvailability = when {
    serverApiReachable == false || carrier == CarrierAvailability.UNREACHABLE -> CallAvailability.NO_SERVER
    !voiceRegistered -> CallAvailability.NO_VOICE
    carrier == CarrierAvailability.DOWN -> CallAvailability.NO_CARRIER
    carrier != CarrierAvailability.READY || serverApiReachable != true -> CallAvailability.CONNECTING
    else -> CallAvailability.READY
}

// Carrier outages are announced by server-originated SIP calls to all clients.
// Only loss of this device's own server connection needs a local audible alarm.
internal fun CallAvailability.needsLocalConnectionAlert(): Boolean =
    this == CallAvailability.NO_SERVER || this == CallAvailability.NO_VOICE

/** Polls carrier readiness natively, even when the UI/WebView is suspended. */
internal fun CoroutineScope.launchCarrierStatusSupervisor(
    settings: Flow<ServerSettings>,
    load: suspend (ServerSettings) -> RuntimeStatus,
    onState: (CarrierAvailability) -> Unit,
    pollIntervalMs: Long = 10_000,
    requestTimeoutMs: Long = 10_000,
): Job = launch {
    settings.distinctUntilChanged().collectLatest { currentSettings ->
        onState(CarrierAvailability.UNKNOWN)
        while (true) {
            val state = try {
                val status = withTimeout(requestTimeoutMs) { load(currentSettings) }
                currentCoroutineContext().ensureActive()
                // SIM access is not required while the established carrier path works.
                if (status.ready && status.tunnelReady && status.imsReady) CarrierAvailability.READY
                else CarrierAvailability.DOWN
            } catch (error: Exception) {
                // A request timeout means unavailable; cancellation of this supervisor
                // (including a settings change) must never publish an obsolete result.
                currentCoroutineContext().ensureActive()
                if (error is CancellationException && error !is kotlinx.coroutines.TimeoutCancellationException) throw error
                CarrierAvailability.UNREACHABLE
            }
            onState(state)
            delay(pollIntervalMs)
        }
    }
}
