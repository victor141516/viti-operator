package com.viti.operator.voice

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.launch

internal fun effectiveServerConnection(
    voiceRegistered: Boolean,
    serverApiReachable: Boolean?,
): Boolean = voiceRegistered && serverApiReachable != false

/**
 * Debounces voice-registration loss before escalating it to a local alert.
 *
 * Registration is the capability that matters here: a device with ordinary
 * Internet access but no Viti voice registration still cannot receive calls.
 */
internal fun CoroutineScope.launchServerConnectionAlertMonitor(
    connectionAvailable: Flow<Boolean>,
    showAlert: () -> Unit,
    dismissAlert: () -> Unit,
    initialGraceMs: Long = 30_000L,
    reconnectGraceMs: Long = 15_000L,
): Job = launch {
    var connectedOnce = false
    connectionAvailable.distinctUntilChanged().collectLatest { available ->
        if (available) {
            connectedOnce = true
            dismissAlert()
        } else {
            delay(if (connectedOnce) reconnectGraceMs else initialGraceMs)
            showAlert()
        }
    }
}
