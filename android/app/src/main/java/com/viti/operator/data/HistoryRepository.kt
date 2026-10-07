package com.viti.operator.data

import android.content.Context
import android.util.AtomicFile
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.delay
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.selects.select
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.io.IOException
import java.security.MessageDigest
import java.util.concurrent.TimeUnit

data class HistoryState(
    val source: String = "",
    val messages: List<Message> = emptyList(),
    val calls: List<Call> = emptyList(),
    val contactsVersion: Long = 0,
    val loaded: Boolean = false,
    val connected: Boolean = false,
)

/** The foreground service owns the connection; Activities only observe cache. */
class HistoryRepository internal constructor(context: Context) {
    private val cache = AtomicFile(File(context.applicationContext.noBackupFilesDir, "history-cache-v1.json"))
    private val mutableState = MutableStateFlow(HistoryState())
    val state = mutableState.asStateFlow()
    private val invalidations = MutableSharedFlow<Unit>(extraBufferCapacity = 1)
    val metadataChanges = invalidations.asSharedFlow()
    private val checks = Channel<Unit>(Channel.CONFLATED)
    private var owner: Job? = null

    fun requestCheck() { checks.trySend(Unit) }
    fun notifyMetadataChanged() { invalidations.tryEmit(Unit) }

    fun start(scope: CoroutineScope, settings: Flow<ServerSettings>, onIncoming: (Message) -> Unit) {
        if (owner?.isActive == true) return
        owner = scope.launch(Dispatchers.IO) {
            settings.distinctUntilChanged().collectLatest { currentSettings ->
                val source = sourceKey(currentSettings)
                var checkpoint = loadCache(source)
                publish(currentSettings, checkpoint, connected = false)
                val endpoint = runCatching { (normalizeServerUrl(currentSettings.serverUrl) + "/api/v1/history/ws").toHttpUrl() }.getOrNull()
                    ?: awaitCancellation()
                val http = OkHttpClient.Builder().pingInterval(25, TimeUnit.SECONDS)
                    .connectTimeout(10, TimeUnit.SECONDS).readTimeout(0, TimeUnit.MILLISECONDS)
                    .followRedirects(false).followSslRedirects(false).build()
                var retryMs = 1_000L
                try {
                    while (currentCoroutineContext().isActive) {
                        val inputs = Channel<String>(64)
                        val url = endpoint.newBuilder().addQueryParameter("cursor", checkpoint.cursor).build()
                        val request = Request.Builder().url(url).apply {
                            if (currentSettings.apiKey.isNotBlank()) header("Authorization", "Bearer ${currentSettings.apiKey}")
                        }.build()
                        val socket = http.newWebSocket(request, object : WebSocketListener() {
                            override fun onMessage(webSocket: WebSocket, text: String) {
                                if (text.length > MAX_FRAME_CHARS || !inputs.trySend(text).isSuccess) {
                                    inputs.close(IOException("History stream overflow")); webSocket.cancel()
                                }
                            }
                            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                                inputs.close(IOException("History connection interrupted"))
                            }
                            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) { inputs.close() }
                            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                                webSocket.close(code, null); inputs.close()
                            }
                        })
                        var initialized = false
                        try {
                            while (currentCoroutineContext().isActive) {
                                select<Unit> {
                                    checks.onReceive {
                                        if (checkpoint.cursor.isBlank()) return@onReceive
                                        if (!socket.send(JSONObject().put("type", "sync").put("cursor", checkpoint.cursor).toString())) {
                                            throw IOException("History connection unavailable")
                                        }
                                    }
                                    inputs.onReceive { text ->
                                        val body = JSONObject(text)
                                        if (body.optString("type") == "history.invalidate") {
                                            invalidations.tryEmit(Unit)
                                        } else {
                                            require(body.getString("type") == "history.sync" && body.getInt("version") == 1)
                                            val patch = parsePatch(body)
                                            val updated = try { applyHistoryPatch(checkpoint, patch) } catch (_: IllegalArgumentException) {
                                                // Ask for an authoritative replacement on any gap. Never
                                                // silently skip a batch or advance a divergent checkpoint.
                                                if (!socket.send(JSONObject().put("type", "sync").put("cursor", "").toString())) throw IOException("History resync unavailable")
                                                return@onReceive
                                            }
                                            currentCoroutineContext().ensureActive()
                                            if (updated != checkpoint) {
                                                // Disk exhaustion must not cause a full-download reconnect
                                                // loop. Keep a coherent in-memory checkpoint; a later app
                                                // restart can replay from the last atomic disk checkpoint.
                                                try { saveCache(source, updated) } catch (_: IOException) { }
                                            }
                                            currentCoroutineContext().ensureActive()
                                            val known = checkpoint.messages.keys
                                            val hadBaseline = checkpoint.cursor.isNotBlank()
                                            checkpoint = updated
                                            publish(currentSettings, checkpoint, connected = true)
                                            if (!initialized) { initialized = true; invalidations.tryEmit(Unit) }
                                            if (hadBaseline) mutableState.value.messages.filter { it.direction == "incoming" && it.id !in known }
                                                .sortedBy { it.createdAt }.forEach(onIncoming)
                                            retryMs = 1_000L
                                        }
                                    }
                                }
                            }
                        } catch (error: CancellationException) { throw error
                        } catch (_: Exception) {
                            mutableState.value = mutableState.value.copy(connected = false)
                        } finally { socket.cancel(); inputs.close() }
                        delay(retryMs)
                        retryMs = (retryMs * 2).coerceAtMost(30_000)
                    }
                } finally {
                    http.dispatcher.executorService.shutdown(); http.connectionPool.evictAll()
                }
            }
        }
    }

    fun stop() { owner?.cancel(); owner = null; mutableState.value = mutableState.value.copy(connected = false) }

    private fun publish(settings: ServerSettings, checkpoint: HistoryCheckpoint, connected: Boolean) {
        val api = VitiApi(settings)
        mutableState.value = HistoryState(sourceKey(settings),
            checkpoint.messages.values.map { api.messageFromJson(JSONObject(it)) }.sortedByDescending { it.createdAt },
            checkpoint.calls.values.map { api.callFromJson(JSONObject(it)) }.sortedByDescending { it.startedAt },
            checkpoint.contactsVersion, checkpoint.cursor.isNotBlank(), connected)
    }

    private fun parsePatch(body: JSONObject) = HistoryPatch(
        mode = body.getString("mode"), baseCursor = body.getString("base_cursor"), cursor = body.getString("cursor"),
        messages = rows(body.getJSONArray("messages")), calls = rows(body.getJSONArray("calls")),
        deletedMessages = ids(body.getJSONArray("deleted_messages")), deletedCalls = ids(body.getJSONArray("deleted_calls")),
        contactsVersion = body.getLong("contacts_version"),
    )

    private fun rows(array: JSONArray): Map<String, String> = buildMap {
        for (index in 0 until array.length()) {
            val row = array.getJSONObject(index); val id = row.getString("id")
            require(id.isNotBlank() && !containsKey(id)); put(id, row.toString())
        }
    }
    private fun ids(array: JSONArray): Set<String> = (0 until array.length()).map { array.getString(it) }.toSet()

    private fun saveCache(source: String, checkpoint: HistoryCheckpoint) {
        val payload = JSONObject().put("schema", 1).put("source", source).put("cursor", checkpoint.cursor)
            .put("messages", JSONObject(checkpoint.messages)).put("calls", JSONObject(checkpoint.calls))
            .put("contacts_version", checkpoint.contactsVersion).toString()
        val bytes = JSONObject().put("payload", payload).put("sha256", digest(payload)).toString().toByteArray(Charsets.UTF_8)
        val output = cache.startWrite()
        try { output.write(bytes); cache.finishWrite(output) } catch (error: Exception) { cache.failWrite(output); throw error }
    }

    private fun loadCache(source: String): HistoryCheckpoint = runCatching {
        val envelope = JSONObject(cache.openRead().use { it.readBytes().toString(Charsets.UTF_8) })
        val payload = envelope.getString("payload")
        require(envelope.getString("sha256") == digest(payload))
        val body = JSONObject(payload)
        require(body.getInt("schema") == 1 && body.getString("source") == source)
        fun strings(key: String): Map<String, String> {
            val values = body.getJSONObject(key)
            return values.keys().asSequence().associateWith { values.getString(it) }
        }
        val checkpoint = HistoryCheckpoint(body.getString("cursor"), strings("messages"), strings("calls"), body.getLong("contacts_version"))
        // Validate both cursor and cached row identities before trusting a checkpoint.
        applyHistoryPatch(HistoryCheckpoint(), HistoryPatch("full", "", checkpoint.cursor, checkpoint.messages, checkpoint.calls))
        for ((id, text) in checkpoint.messages + checkpoint.calls) require(JSONObject(text).getString("id") == id)
        checkpoint
    }.getOrDefault(HistoryCheckpoint())

    companion object {
        private const val MAX_FRAME_CHARS = 16 * 1024 * 1024
        @Volatile private var instance: HistoryRepository? = null
        fun get(context: Context): HistoryRepository = instance ?: synchronized(this) {
            instance ?: HistoryRepository(context).also { instance = it }
        }
        private fun digest(value: String) = MessageDigest.getInstance("SHA-256")
            .digest(value.toByteArray(Charsets.UTF_8)).joinToString("") { "%02x".format(it) }
        fun sourceKey(settings: ServerSettings): String = digest(normalizeServerUrl(settings.serverUrl) + "\u0000" + settings.apiKey)
    }
}
