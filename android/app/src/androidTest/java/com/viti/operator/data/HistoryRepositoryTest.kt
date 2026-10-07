package com.viti.operator.data

import android.content.ContextWrapper
import androidx.test.platform.app.InstrumentationRegistry
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.mockwebserver.MockResponse
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.io.File
import java.util.UUID
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference

class HistoryRepositoryTest {
    private fun context() = object : ContextWrapper(InstrumentationRegistry.getInstrumentation().targetContext) {
        private val directory = File(baseContext.cacheDir, "history-fixture-${UUID.randomUUID()}").apply { mkdirs() }
        override fun getApplicationContext() = this
        override fun getNoBackupFilesDir() = directory
    }
    private fun frame(mode: String, base: String, revision: Int, messages: String = "[]", calls: String = "[]") = JSONObject()
        .put("type", "history.sync").put("version", 1).put("mode", mode).put("base_cursor", base)
        .put("cursor", "history-fixture:$revision").put("messages", JSONArray(messages)).put("calls", JSONArray(calls))
        .put("deleted_messages", JSONArray()).put("deleted_calls", JSONArray()).put("contacts_version", 0).toString()

    @Test fun pushOldRowUpdateIdleCheckAndDurableResume() = runBlocking {
        val server = okhttp3.mockwebserver.MockWebServer()
        val socket = AtomicReference<WebSocket>()
        val commands = AtomicInteger()
        val clientCursor = AtomicReference<String>()
        val listener = object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                socket.set(webSocket)
                val cursor = server.takeRequest(5, TimeUnit.SECONDS)?.requestUrl?.queryParameter("cursor").orEmpty()
                clientCursor.set(cursor)
                webSocket.send(if (cursor.isEmpty()) frame("full", "", 0,
                    messages = """[{"id":"older","direction":"incoming","text":"fixture","state":"received"}]""",
                    calls = """[{"id":"call","state":"active","direction":"incoming","started_at":"2026-01-01T00:00:00Z"}]""")
                else frame("delta", cursor, 1))
            }
            override fun onMessage(webSocket: WebSocket, text: String) {
                commands.incrementAndGet()
                val cursor = JSONObject(text).getString("cursor")
                webSocket.send(frame("delta", cursor, if (cursor.endsWith(":0")) 0 else 1))
            }
        }
        repeat(2) { server.enqueue(MockResponse().withWebSocketUpgrade(listener)) }
        server.start()
        val settings = MutableStateFlow(ServerSettings(server.url("/").toString()))
        val appContext = context()
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)
        val repository = HistoryRepository(appContext)
        val incoming = AtomicInteger()
        try {
            repository.start(scope, settings) { incoming.incrementAndGet() }
            withTimeout(5_000) { repository.state.first { it.connected && it.calls.size == 1 } }
            assertEquals(0, incoming.get()) // Initial history is not a new-message alert.
            delay(500)
            assertEquals(1, server.requestCount) // No periodic HTTP history downloads.
            assertEquals(0, commands.get())
            socket.get().send(frame("delta", "history-fixture:0", 1,
                messages = """[{"id":"new","direction":"incoming","text":"fixture"}]""",
                calls = """[{"id":"call","state":"ended","direction":"incoming","started_at":"2026-01-01T00:00:00Z","ended_at":"2026-01-01T00:01:00Z"}]"""))
            withTimeout(5_000) { repository.state.first { it.messages.size == 2 && it.calls.first().state == "ended" } }
            withTimeout(5_000) { while (incoming.get() != 1) delay(10) }
            repository.requestCheck()
            withTimeout(5_000) { while (commands.get() != 1) delay(10) }
            assertEquals(1, server.requestCount)
            repository.stop()
            val restarted = HistoryRepository(appContext)
            restarted.start(scope, settings) { incoming.incrementAndGet() }
            withTimeout(5_000) { restarted.state.first { it.connected && it.messages.size == 2 } }
            assertEquals("history-fixture:1", clientCursor.get())
            assertEquals("ended", restarted.state.value.calls.first().state)
            assertEquals(1, incoming.get())
            restarted.stop()
        } finally { repository.stop(); scope.cancel(); server.shutdown() }
    }

    @Test fun gapRequestsFullReplacementAndCorruptCacheIsNotTrusted() = runBlocking {
        val server = okhttp3.mockwebserver.MockWebServer()
        val requests = AtomicInteger()
        val initialCursor = AtomicReference<String>()
        val listener = object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                initialCursor.set(server.takeRequest(5, TimeUnit.SECONDS)?.requestUrl?.queryParameter("cursor"))
                webSocket.send(frame("full", "", 0))
                webSocket.send(frame("delta", "history-fixture:99", 100))
            }
            override fun onMessage(webSocket: WebSocket, text: String) {
                assertEquals("", JSONObject(text).getString("cursor"))
                requests.incrementAndGet(); webSocket.send(frame("full", "", 100,
                    calls = """[{"id":"recovered","state":"ended"}]"""))
            }
        }
        server.enqueue(MockResponse().withWebSocketUpgrade(listener)); server.start()
        val appContext = context()
        File(appContext.noBackupFilesDir, "history-cache-v1.json").writeText("corrupt fixture")
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)
        val repository = HistoryRepository(appContext)
        try {
            repository.start(scope, MutableStateFlow(ServerSettings(server.url("/").toString()))) { }
            withTimeout(5_000) { repository.state.first { it.calls.firstOrNull()?.id == "recovered" } }
            assertEquals(1, requests.get())
            assertEquals("", initialCursor.get())
        } finally { repository.stop(); scope.cancel(); server.shutdown() }
    }

    @Test fun accountChangeDoesNotReuseAnotherSourcesCache() = runBlocking {
        val server = okhttp3.mockwebserver.MockWebServer()
        val cursors = java.util.Collections.synchronizedList(mutableListOf<String>())
        repeat(2) { index -> server.enqueue(MockResponse().withWebSocketUpgrade(object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                cursors.add(server.takeRequest(5, TimeUnit.SECONDS)?.requestUrl?.queryParameter("cursor").orEmpty())
                webSocket.send(frame("full", "", index, messages = if (index == 0) """[{"id":"private-first-source"}]""" else "[]"))
            }
        })) }
        server.start()
        val settings = MutableStateFlow(ServerSettings(server.url("/").toString(), "first-fixture-key"))
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)
        val repository = HistoryRepository(context())
        try {
            repository.start(scope, settings) { }
            withTimeout(5_000) { repository.state.first { it.connected && it.messages.size == 1 } }
            settings.value = settings.value.copy(apiKey = "second-fixture-key")
            val secondSource = HistoryRepository.sourceKey(settings.value)
            withTimeout(5_000) { repository.state.first { it.connected && it.source == secondSource } }
            assertTrue(repository.state.value.messages.isEmpty())
            assertEquals(listOf("", ""), cursors.toList())
        } finally { repository.stop(); scope.cancel(); server.shutdown() }
    }

    @Test fun cacheWriteFailureDoesNotCauseDownloadReconnectStorm() = runBlocking {
        val parent = InstrumentationRegistry.getInstrumentation().targetContext
        val appContext = object : ContextWrapper(parent) {
            private val notDirectory = File(baseContext.cacheDir, "history-unwritable-${UUID.randomUUID()}").apply { writeText("fixture") }
            override fun getApplicationContext() = this
            override fun getNoBackupFilesDir() = notDirectory
        }
        val server = okhttp3.mockwebserver.MockWebServer()
        server.enqueue(MockResponse().withWebSocketUpgrade(object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                webSocket.send(frame("full", "", 0, messages = """[{"id":"memory-only"}]"""))
            }
        })); server.start()
        val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main)
        val repository = HistoryRepository(appContext)
        try {
            repository.start(scope, MutableStateFlow(ServerSettings(server.url("/").toString()))) { }
            withTimeout(5_000) { repository.state.first { it.connected && it.messages.size == 1 } }
            delay(1_200)
            assertTrue(repository.state.value.connected)
            assertEquals(1, server.requestCount)
        } finally { repository.stop(); scope.cancel(); server.shutdown() }
    }
}
