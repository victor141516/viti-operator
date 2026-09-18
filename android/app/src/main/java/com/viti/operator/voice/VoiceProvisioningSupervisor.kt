package com.viti.operator.voice

import com.viti.operator.data.ServerSettings
import com.viti.operator.data.VoiceProvisioning
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

/** Keeps process-owned SIP provisioning independent from every UI lifecycle. */
internal fun CoroutineScope.launchVoiceProvisioningSupervisor(
    settings: Flow<ServerSettings>,
    load: suspend (ServerSettings) -> VoiceProvisioning,
    apply: (VoiceProvisioning) -> Unit,
    onFailure: (Throwable) -> Unit = {},
    onReachabilityChanged: (Boolean) -> Unit = {},
    retryDelayMs: Long = 5_000,
    refreshDelayMs: Long = 30_000,
): Job = launch {
    var appliedProvisioning: VoiceProvisioning? = null
    settings.distinctUntilChanged().collectLatest { currentSettings ->
        while (true) {
            try {
                val provisioning = load(currentSettings)
                currentCoroutineContext().ensureActive()
                onReachabilityChanged(true)
                if (provisioning != appliedProvisioning) {
                    apply(provisioning)
                    appliedProvisioning = provisioning
                }
                delay(refreshDelayMs)
            } catch (error: CancellationException) {
                throw error
            } catch (error: Throwable) {
                onReachabilityChanged(false)
                onFailure(error)
                delay(retryDelayMs)
            }
        }
    }
}
