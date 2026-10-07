package com.viti.operator.voice

import android.Manifest
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.viti.operator.data.ClientIdentity
import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.*
import kotlinx.coroutines.channels.Channel
import okhttp3.Credentials
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.junit.Assert.*
import org.junit.Assume.assumeTrue
import org.junit.Test
import org.junit.runner.RunWith

/** Opt-in against the Asterisk-only fixture, never a real operator or phone. */
@RunWith(AndroidJUnit4::class)
class NativeAsteriskInteropTest {
    @Test fun nativeRegistrationEchoIncomingAnswerAndRemoteHangupInteroperateWithAsterisk() = runBlocking {
        val url = InstrumentationRegistry.getArguments().getString("native_lab_url").orEmpty()
        assumeTrue("An isolated native voice lab URL is required", url.matches(Regex("ws://(?:10\\.0\\.2\\.2|127\\.0\\.0\\.1):[0-9]+/ws")))
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val context = instrumentation.targetContext
        instrumentation.uiAutomation.grantRuntimePermission(context.packageName, Manifest.permission.RECORD_AUDIO)
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
        val registrations = Channel<Unit>(Channel.UNLIMITED)
        val incoming = Channel<VoiceEngineEvent>(Channel.UNLIMITED)
        val active = Channel<VoiceEngineEvent>(Channel.UNLIMITED)
        val ended = Channel<VoiceEngineEvent>(Channel.UNLIMITED)
        val audio = Channel<Unit>(Channel.CONFLATED)
        val http = OkHttpClient()
        val client = withContext(Dispatchers.Main) {
            NativeVoiceClient(context, ClientIdentity("isolated-native-client", "Isolated native lab", false), scope) {
                when (it.type) {
                    "registered" -> registrations.trySend(Unit)
                    "incoming" -> incoming.trySend(it)
                    "active" -> active.trySend(it)
                    "ended", "failed", "interrupted" -> ended.trySend(it)
                    "media" -> if (it.cause == "sent=true,received=true") audio.trySend(Unit)
                }
            }.also { it.start(VoiceProvisioning(true, url, "lab.invalid", "native-lab", "native-lab-only")) }
        }
        try {
            withTimeout(20_000) { registrations.receive() }
            withContext(Dispatchers.Main) { client.call("isolated-echo-leg", "*43", "isolated-echo-trace") }
            val echo = withTimeout(20_000) { active.receive() }
            assertEquals("isolated-echo-leg", echo.legToken)
            withTimeout(15_000) { audio.receive() }
            withContext(Dispatchers.Main) { client.sendDtmf(echo.legToken, '5'); client.hangup(echo.legToken) }
            assertEquals("ended", withTimeout(5000) { ended.receive() }.type)
            withContext(Dispatchers.IO) {
                val endpoint = url.replaceFirst("ws://", "http://").removeSuffix("/ws") +
                    "/ari/channels?endpoint=PJSIP%2Fnative-lab&extension=*45&context=native-lab&priority=1"
                http.newCall(Request.Builder().url(endpoint)
                    .header("Authorization", Credentials.basic("native-lab", "native-lab-only"))
                    .post(ByteArray(0).toRequestBody()).build()).execute().use { assertTrue("Lab originate failed", it.isSuccessful) }
            }
            val invitation = withTimeout(10_000) { incoming.receive() }
            withContext(Dispatchers.Main) { client.answer(invitation.legToken) }
            assertEquals(invitation.legToken, withTimeout(10_000) { active.receive() }.legToken)
            val terminal = withTimeout(10_000) { ended.receive() }
            assertEquals("A remote BYE must end the call, not request media recovery", "ended", terminal.type)
            assertEquals(invitation.legToken, terminal.legToken)
        } finally {
            withContext(Dispatchers.Main) { client.destroy() }
            scope.cancel(); http.dispatcher.executorService.shutdown(); http.connectionPool.evictAll()
            registrations.close(); incoming.close(); active.close(); ended.close(); audio.close()
        }
    }
}
