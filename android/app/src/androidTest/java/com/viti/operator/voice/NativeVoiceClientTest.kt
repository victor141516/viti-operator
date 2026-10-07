package com.viti.operator.voice

import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.viti.operator.data.ClientIdentity
import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.*
import kotlinx.coroutines.channels.Channel
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class NativeVoiceClientTest {
    @Test fun nativeRegistrationRenewsAndIncomingCallsAreDeliveredWithoutCreatingAnyUi() = runBlocking {
        val registered = Channel<Unit>(Channel.UNLIMITED)
        val incoming = CompletableDeferred<VoiceEngineEvent>()
        val ended = CompletableDeferred<Unit>()
        val serverPeer = CompletableDeferred<WebSocket>()
        val server = MockWebServer()
        server.enqueue(MockResponse().setHeader("Sec-WebSocket-Protocol", "sip").withWebSocketUpgrade(object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) { serverPeer.complete(webSocket) }
            override fun onMessage(webSocket: WebSocket, text: String) {
                val request = SipMessage.parse(text) ?: return
                if (request.method == "REGISTER" && request.status == 0) {
                    webSocket.send(SipMessage.response(request, 200, extra = listOf("Contact" to request.header("Contact").replace("expires=600", "expires=10"))).encode())
                }
            }
        }))
        server.start()
        val url = server.url("/sipws").toString().replaceFirst("http://", "ws://")
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
        val client = withContext(Dispatchers.Main) {
            NativeVoiceClient(InstrumentationRegistry.getInstrumentation().targetContext, ClientIdentity("test-native", "Local native test", false), scope) {
                if (it.type == "registered") registered.trySend(Unit)
                if (it.type == "incoming") incoming.complete(it)
                if (it.type == "ended") ended.complete(Unit)
            }.also { it.start(VoiceProvisioning(true, url, "example.invalid", "test", "local-test-only")) }
        }
        try {
            withTimeout(20_000) { repeat(3) { registered.receive() } }
            val invite = SipMessage("INVITE sip:test@example.invalid SIP/2.0", listOf(
                "Via" to "SIP/2.0/WS server.invalid;branch=z9hG4bKlocaltest", "From" to "<sip:local@example.invalid>;tag=remote",
                "To" to "<sip:test@example.invalid>", "Call-ID" to "local-incoming", "CSeq" to "1 INVITE", "Contact" to "<sip:local@server.invalid>",
                "Content-Type" to "application/sdp", "X-Viti-Trace-ID" to "local-trace"), "local-offer")
            serverPeer.await().send(invite.encode())
            val event = withTimeout(5000) { incoming.await() }
            assertEquals("local-trace", event.traceId)
            withContext(Dispatchers.Main) { client.reject(event.legToken) }
            withTimeout(5000) { ended.await() }
            assertTrue(client.maintenanceDispatchCount >= 8)
            assertEquals("open", client.nativeSipSocket.stats().state)
        } finally {
            withContext(Dispatchers.Main) { client.destroy() }
            scope.cancel(); server.shutdown(); registered.close()
        }
    }
}
