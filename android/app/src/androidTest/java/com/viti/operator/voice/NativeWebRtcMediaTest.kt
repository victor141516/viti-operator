package com.viti.operator.voice

import android.Manifest
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.*
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class NativeWebRtcMediaTest {
    @Test fun nativePeersExchangeEncryptedAudioAndDtmfWithoutAnActivityOrWebView() = runBlocking {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val context = instrumentation.targetContext
        instrumentation.uiAutomation.grantRuntimePermission(context.packageName, Manifest.permission.RECORD_AUDIO)
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
        val connectedA = CompletableDeferred<Unit>()
        val connectedB = CompletableDeferred<Unit>()
        val receivedA = CompletableDeferred<Unit>()
        val receivedB = CompletableDeferred<Unit>()
        val (a, b) = withContext(Dispatchers.Main) {
            NativeWebRtcMedia(context, VoiceProvisioning(), scope) {
                if (it.state == "connected") connectedA.complete(Unit)
                if (it.received) receivedA.complete(Unit)
            } to NativeWebRtcMedia(context, VoiceProvisioning(), scope) {
                if (it.state == "connected") connectedB.complete(Unit)
                if (it.received) receivedB.complete(Unit)
            }
        }
        try {
            withContext(Dispatchers.Main) {
                val offer = a.offer()
                assertTrue(offer.contains("a=fingerprint:"))
                assertTrue(offer.contains("a=rtcp-mux"))
                val answer = b.answer(offer)
                a.acceptAnswer(answer)
            }
            withTimeout(15_000) { connectedA.await(); connectedB.await(); receivedA.await(); receivedB.await() }
            withContext(Dispatchers.Main) {
                a.setAudioState(true, false)
                a.setAudioState(false, false)
                a.dtmf('5')
                a.setAudioState(true, true)
                a.setAudioState(false, false)
            }
        } finally {
            withContext(Dispatchers.Main) { a.close(); b.close() }
            scope.cancel()
        }
    }
}
