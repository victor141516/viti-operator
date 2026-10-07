package com.viti.operator.voice

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.delay
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.launch

/** Native scheduling continues independently of WebView registration timers. */
internal fun CoroutineScope.launchVoiceSignalingSupervisor(
    maintain: suspend () -> Unit,
    onFailure: (Throwable) -> Unit,
    release: () -> Unit = {},
    intervalMs: Long = 30_000,
): Job = launch {
    try {
        while (true) {
            try {
                maintain()
            } catch (error: Exception) {
                currentCoroutineContext().ensureActive()
                onFailure(error)
            }
            delay(intervalMs)
        }
    } finally {
        release()
    }
}
