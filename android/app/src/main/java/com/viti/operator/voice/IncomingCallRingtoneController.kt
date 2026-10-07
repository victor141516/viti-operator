package com.viti.operator.voice

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/** Ringtone ownership belongs to the service, never to an Activity or FSI. */
internal class IncomingCallRingtoneController(
    private val start: () -> Unit,
    private val stop: () -> Unit,
    private val scope: CoroutineScope? = null,
    private val startDelayMs: Long = 0,
) {
    private var ringingToken = ""
    private var pendingStart: Job? = null

    fun update(snapshot: VoiceCallSnapshot) {
        val next = snapshot.legToken.takeIf { shouldShowIncomingCall(snapshot, it) }.orEmpty()
        if (ringingToken == next) return
        pendingStart?.cancel()
        pendingStart = null
        if (ringingToken.isNotBlank()) stop()
        ringingToken = next
        if (next.isNotBlank()) {
            if (scope == null) start()
            else pendingStart = scope.launch {
                delay(startDelayMs)
                if (ringingToken == next) start()
            }
        }
    }

    fun close() {
        ringingToken = ""
        pendingStart?.cancel()
        pendingStart = null
        stop()
    }
}
