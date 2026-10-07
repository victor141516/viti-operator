package com.viti.operator.voice

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.media.AudioManager
import android.media.ToneGenerator
import androidx.core.content.ContextCompat
import com.viti.operator.data.ClientIdentity
import com.viti.operator.data.VoiceProvisioning
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import okhttp3.OkHttpClient
import java.util.concurrent.TimeUnit

/** Entire voice engine belongs to the foreground service, with no UI dependency. */
class NativeVoiceClient internal constructor(
    context: Context,
    identity: ClientIdentity,
    private val scope: CoroutineScope,
    private val onEvent: (VoiceEngineEvent) -> Unit,
) : VoiceEngineControl {
    private val applicationContext = context.applicationContext
    private val http = OkHttpClient.Builder().pingInterval(25, TimeUnit.SECONDS)
        .connectTimeout(10, TimeUnit.SECONDS).readTimeout(0, TimeUnit.MILLISECONDS)
        .followRedirects(false).followSslRedirects(false).build()
    private lateinit var engine: NativeSipEngine
    internal val nativeSipSocket = NativeSipWebSocket(http) { event -> scope.launch { engine.socketEvent(event) } }
    private var supervisor: Job? = null
    private var ringback: ToneGenerator? = null
    private var destroyed = false
    val signalingEnabled get() = engine.enabled
    var maintenanceDispatchCount = 0L
        private set

    init {
        engine = NativeSipEngine(scope, identity.id, identity.name,
            transport = { action, id, value ->
                when (action) {
                    "connect" -> nativeSipSocket.connect(id, value)
                    "send" -> nativeSipSocket.send(id, value)
                    "close" -> { nativeSipSocket.disconnect(id); true }
                    else -> false
                }
            },
            mediaFactory = { provisioning, callback -> NativeWebRtcMedia(applicationContext, provisioning, scope, callback) },
            onEvent = { event ->
                if (event.type == "ringing") startRingback()
                else if (event.type in setOf("active", "ended", "failed", "interrupted", "disconnected")) stopRingback()
                onEvent(event)
            })
    }

    override fun hasMicrophonePermission() = ContextCompat.checkSelfPermission(applicationContext, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED
    override fun start(provisioning: VoiceProvisioning) {
        if (destroyed) return
        engine.start(provisioning)
        if (!provisioning.enabled) { supervisor?.cancel(); supervisor = null; stopRingback(); return }
        if (supervisor?.isActive == true) return
        supervisor = scope.launch {
            while (!destroyed && engine.enabled) {
                maintainRegistration()
                delay(1000)
            }
        }
    }
    fun maintainRegistration() {
        if (destroyed) return
        maintenanceDispatchCount++
        engine.maintain()
    }
    override suspend fun call(legToken: String, number: String, traceId: String) { engine.call(legToken, number, traceId) }
    override suspend fun answer(legToken: String) { engine.answer(legToken) }
    override suspend fun reject(legToken: String) { engine.end(legToken, true) }
    override suspend fun hangup(legToken: String) { engine.end(legToken) }
    override suspend fun reconnect(legToken: String, traceId: String) { engine.call(legToken, "*44", traceId, true) }
    override suspend fun setMuted(legToken: String, muted: Boolean) { engine.mute(legToken, muted) }
    override suspend fun setTelecomMuted(legToken: String, muted: Boolean) { engine.telecomMute(legToken, muted) }
    override suspend fun setSystemInactive(legToken: String, inactive: Boolean) { engine.inactive(legToken, inactive) }
    override suspend fun sendDtmf(legToken: String, tone: Char) { engine.dtmf(legToken, tone) }
    override fun destroy() {
        if (destroyed) return
        destroyed = true; supervisor?.cancel(); stopRingback()
        engine.close(); nativeSipSocket.close()
        http.dispatcher.executorService.shutdown(); http.connectionPool.evictAll()
    }
    private fun startRingback() {
        if (ringback != null) return
        ringback = runCatching { ToneGenerator(AudioManager.STREAM_VOICE_CALL, 40).also { it.startTone(ToneGenerator.TONE_SUP_RINGTONE) } }.getOrNull()
    }
    private fun stopRingback() { ringback?.stopTone(); ringback?.release(); ringback = null }
}
