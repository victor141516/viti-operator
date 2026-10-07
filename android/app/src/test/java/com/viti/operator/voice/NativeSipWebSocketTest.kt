package com.viti.operator.voice

import okhttp3.OkHttpClient
import okhttp3.Protocol
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okio.ByteString
import org.junit.Assert.*
import org.junit.Test
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit

class NativeSipWebSocketTest {
    private fun response(socket: WebSocket, protocol: Boolean = true) = Response.Builder()
        .request(socket.request()).protocol(Protocol.HTTP_1_1).code(101).message("Switching Protocols")
        .apply { if (protocol) header("Sec-WebSocket-Protocol", "sip") }.build()

    private class FakeSocket(private val req: Request) : WebSocket {
        var cancelled = false
        val messages = mutableListOf<String>()
        override fun request() = req
        override fun queueSize() = 0L
        override fun send(text: String): Boolean { messages.add(text); return true }
        override fun send(bytes: ByteString) = true
        override fun close(code: Int, reason: String?) = true
        override fun cancel() { cancelled = true }
    }

    @Test fun replacementAndShutdownFenceOldCallbacksAndCommands() {
        val listeners = mutableListOf<WebSocketListener>()
        val sockets = mutableListOf<FakeSocket>()
        val events = mutableListOf<NativeSipSocketEvent>()
        val factory = WebSocket.Factory { request, listener ->
            listeners.add(listener)
            FakeSocket(request).also { sockets.add(it) }
        }
        val transport = NativeSipWebSocket(factory, events::add)
        assertTrue(transport.connect(1, "ws://example.invalid/sipws"))
        listeners[0].onOpen(sockets[0], response(sockets[0]))
        assertTrue(transport.send(1, "first"))
        assertTrue(transport.connect(2, "ws://example.invalid/sipws"))
        assertTrue(sockets[0].cancelled)
        listeners[0].onMessage(sockets[0], "stale private payload")
        listeners[0].onFailure(sockets[0], IllegalStateException("private error"), null)
        listeners[1].onOpen(sockets[1], response(sockets[1]))
        transport.disconnect(1)
        assertFalse(transport.send(1, "stale"))
        assertTrue(transport.send(2, "second"))
        assertEquals(listOf("open", "open"), events.map { it.type })
        transport.close()
        listeners[1].onMessage(sockets[1], "after shutdown")
        assertTrue(sockets[1].cancelled)
        assertFalse(transport.connect(3, "ws://example.invalid/sipws"))
        assertEquals("closed", transport.stats().state)
        assertEquals(2L, transport.stats().sent)
    }

    @Test fun wrongSubprotocolAndOversizedInputCannotEnterSip() {
        var listener: WebSocketListener? = null
        var socket: FakeSocket? = null
        val events = mutableListOf<NativeSipSocketEvent>()
        val transport = NativeSipWebSocket(WebSocket.Factory { req, callback ->
            listener = callback
            FakeSocket(req).also { socket = it }
        }, events::add)
        assertFalse(transport.connect(0, "ws://example.invalid"))
        assertFalse(transport.connect(1, "https://example.invalid"))
        assertTrue(transport.connect(1, "ws://example.invalid"))
        listener!!.onOpen(socket!!, response(socket!!, false))
        assertEquals(1002, events.single().code)
        assertTrue(transport.connect(2, "ws://example.invalid"))
        listener!!.onOpen(socket!!, response(socket!!))
        assertFalse(transport.send(2, "x".repeat(256 * 1024 + 1)))
        listener!!.onMessage(socket!!, "x".repeat(256 * 1024 + 1))
        assertEquals(1009, events.last().code)
        assertEquals(0L, transport.stats().received)
        transport.close()
    }

    @Test fun realNativeWebSocketExchangesFramesAndRecoversAfterPeerClose() {
        val server = MockWebServer()
        val client = OkHttpClient.Builder().pingInterval(1, TimeUnit.SECONDS).build()
        val events = LinkedBlockingQueue<NativeSipSocketEvent>()
        val received = LinkedBlockingQueue<String>()
        val peers = LinkedBlockingQueue<WebSocket>()
        fun upgrade() = MockResponse().setHeader("Sec-WebSocket-Protocol", "sip")
            .withWebSocketUpgrade(object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) { peers.add(webSocket) }
                override fun onMessage(webSocket: WebSocket, text: String) {
                    received.add(text)
                    webSocket.send("SIP/2.0 200 OK\r\n\r\n")
                }
            })
        val transport = NativeSipWebSocket(client) { events.add(it) }
        try {
            server.enqueue(upgrade())
            server.enqueue(upgrade())
            server.start()
            val url = server.url("/sipws").toString().replaceFirst("http://", "ws://")
            assertTrue(transport.connect(1, url))
            assertEquals("open", events.poll(5, TimeUnit.SECONDS)?.type)
            assertTrue(transport.send(1, "REGISTER local test"))
            assertEquals("REGISTER local test", received.poll(5, TimeUnit.SECONDS))
            assertEquals("message", events.poll(5, TimeUnit.SECONDS)?.type)
            assertEquals(1L, transport.stats().received)
            peers.poll(5, TimeUnit.SECONDS)!!.close(1000, "Local test complete")
            val closed = events.poll(5, TimeUnit.SECONDS)!!
            assertEquals("closed", closed.type)
            assertFalse(closed.error)
            assertTrue(transport.connect(2, url))
            assertEquals(2L, events.poll(5, TimeUnit.SECONDS)?.id)
            assertEquals("open", transport.stats().state)
        } finally {
            transport.close()
            client.dispatcher.executorService.shutdown()
            client.connectionPool.evictAll()
            server.shutdown()
        }
    }
}
