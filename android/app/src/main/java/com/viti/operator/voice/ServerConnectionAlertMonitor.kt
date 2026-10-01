package com.viti.operator.voice

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.launch

/**
 * Debounces this device's server connection loss before a local alert.
 *
 * Carrier-only outages must not enter this monitor: the server rings all
 * clients through SIP. Local alerts cover loss of voice registration or API.
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
