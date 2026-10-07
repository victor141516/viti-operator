package com.viti.operator.voice

import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString

internal data class NativeSipSocketEvent(
    val id: Long, val type: String, val message: String = "", val error: Boolean = false, val code: Int = 1000,
)

internal data class NativeSipSocketStats(val state: String, val sent: Long, val received: Long)

/** Generation-fenced, process-owned SIP transport. Never log URLs or SIP frames. */
internal class NativeSipWebSocket(
    private val factory: WebSocket.Factory,
    private val onEvent: (NativeSipSocketEvent) -> Unit,
) : AutoCloseable {
    private class Connection(val id: Long) {
        var socket: WebSocket? = null
        var open = false
    }
    private val lock = Any()
    private var current: Connection? = null
    private var destroyed = false
    private var sent = 0L
    private var received = 0L

    fun stats(): NativeSipSocketStats = synchronized(lock) {
        NativeSipSocketStats(if (current?.open == true) "open" else if (current != null) "connecting" else "closed", sent, received)
    }

    fun connect(id: Long, url: String): Boolean {
        if (id <= 0 || !(url.startsWith("ws://") || url.startsWith("wss://"))) return false
        val request = runCatching { Request.Builder().url(url).header("Sec-WebSocket-Protocol", "sip").build() }.getOrNull() ?: return false
        synchronized(lock) {
            if (destroyed) return false
            val old = current
            val connection = Connection(id)
            current = connection
            old?.socket?.cancel()
            return try {
                connection.socket = factory.newWebSocket(request, listener(connection))
                true
            } catch (_: Exception) {
                if (current === connection) current = null
                false
            }
        }
    }

    fun send(id: Long, message: String): Boolean = synchronized(lock) {
        val connection = current ?: return false
        if (connection.id != id || !connection.open || message.length > MAX_FRAME_CHARS) return false
        val accepted = connection.socket?.send(message) == true
        if (accepted) sent++
        accepted
    }

    fun disconnect(id: Long) {
        synchronized(lock) {
            val connection = current ?: return
            if (connection.id != id) return
            current = null
            connection.socket?.cancel()
        }
    }

    override fun close() {
        synchronized(lock) {
            destroyed = true
            val connection = current
            current = null
            connection?.socket?.cancel()
        }
    }

    private fun listener(connection: Connection) = object : WebSocketListener() {
        override fun onOpen(webSocket: WebSocket, response: Response) {
            if (response.header("Sec-WebSocket-Protocol") != "sip") {
                terminate(connection, true, 1002)
                webSocket.cancel()
                return
            }
            synchronized(lock) {
                if (current !== connection) { webSocket.cancel(); return }
                connection.open = true
                onEvent(NativeSipSocketEvent(connection.id, "open"))
            }
        }

        override fun onMessage(webSocket: WebSocket, text: String) = receive(connection, webSocket, text)
        override fun onMessage(webSocket: WebSocket, bytes: ByteString) = receive(connection, webSocket, bytes.utf8())
        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
            terminate(connection, false, code)
            webSocket.close(code, null)
        }
        override fun onClosed(webSocket: WebSocket, code: Int, reason: String) { terminate(connection, false, code) }
        override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) { terminate(connection, true, 1006) }
    }

    private fun receive(connection: Connection, socket: WebSocket, message: String) {
        if (message.length > MAX_FRAME_CHARS) {
            terminate(connection, true, 1009)
            socket.cancel()
            return
        }
        synchronized(lock) {
            if (current !== connection || !connection.open) return
            received++
            onEvent(NativeSipSocketEvent(connection.id, "message", message))
        }
    }

    private fun terminate(connection: Connection, error: Boolean, code: Int) {
        synchronized(lock) {
            if (current !== connection) return
            current = null
            onEvent(NativeSipSocketEvent(connection.id, "closed", error = error, code = code))
        }
    }

    companion object { private const val MAX_FRAME_CHARS = 256 * 1024 }
}
