package com.viti.operator.voice

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.os.Handler
import android.os.Looper
import android.view.ViewGroup
import android.webkit.ConsoleMessage
import android.webkit.JavascriptInterface
import android.webkit.PermissionRequest
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.core.content.ContextCompat
import androidx.webkit.WebViewAssetLoader
import com.viti.operator.data.ClientIdentity
import com.viti.operator.data.VoiceProvisioning
import com.viti.operator.data.VoiceCallQuality
import com.viti.operator.data.VoiceMediaRoute
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import org.json.JSONObject
import org.json.JSONTokener
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

data class VoiceEngineEvent(
    val type: String,
    val peer: String = "",
    val cause: String = "",
    val traceId: String = "",
    val participantId: String = "",
    val legToken: String = "",
    val quality: VoiceCallQuality? = null,
)

internal object VoiceQualityJson {
    fun parse(body: JSONObject?): VoiceCallQuality? = body?.let {
        val route = it.optJSONObject("route")
        parseValues(
            values = mapOf(
                "sequence" to it.optLong("sequence", 0), "status" to it.optString("status"),
                "sampledAtMs" to it.optLong("sampledAtMs", 0), "inboundLossPercent" to it.optNullableDouble("inboundLossPercent"),
                "outboundLossPercent" to it.optNullableDouble("outboundLossPercent"), "inboundJitterMs" to it.optNullableDouble("inboundJitterMs"),
                "roundTripTimeMs" to it.optNullableDouble("roundTripTimeMs"), "roundTripScope" to it.optString("roundTripScope"),
                "windowMs" to it.optNullableDouble("windowMs"),
            ),
            routeValues = route?.let { value -> mapOf(
                "mode" to value.optString("mode"), "protocol" to value.optString("protocol", "unknown"),
                "localCandidateType" to value.optString("localCandidateType", "unknown"),
                "remoteCandidateType" to value.optString("remoteCandidateType", "unknown"),
            ) },
        )
    }

    private fun JSONObject.optNullableDouble(name: String): Double? =
        if (has(name) && !isNull(name)) optDouble(name).takeIf(Double::isFinite) else null

    fun parseValues(values: Map<String, Any?>, routeValues: Map<String, Any?>? = null): VoiceCallQuality? {
        val sequence = (values["sequence"] as? Number)?.toLong() ?: 0
        val status = values["status"] as? String ?: ""
        if (sequence <= 0 || status !in setOf("connecting", "collecting", "available", "reconnecting", "unavailable", "unsupported")) return null
        val route = routeValues?.let {
            val mode = it["mode"] as? String ?: ""
            if (mode !in setOf("direct", "turn-relay")) null else VoiceMediaRoute(
                mode = mode,
                protocol = it["protocol"] as? String ?: "unknown",
                localCandidateType = it["localCandidateType"] as? String ?: "unknown",
                remoteCandidateType = it["remoteCandidateType"] as? String ?: "unknown",
            )
        }
        fun number(name: String) = (values[name] as? Number)?.toDouble()?.takeIf(Double::isFinite)
        return VoiceCallQuality(
            sequence = sequence,
            status = status,
            sampledAtMs = (values["sampledAtMs"] as? Number)?.toLong() ?: 0,
            inboundLossPercent = number("inboundLossPercent"),
            outboundLossPercent = number("outboundLossPercent"),
            inboundJitterMs = number("inboundJitterMs"),
            roundTripTimeMs = number("roundTripTimeMs"),
            roundTripScope = values["roundTripScope"] as? String ?: "",
            windowMs = number("windowMs"),
            route = route,
        )
    }
}

class VoiceCommandException(message: String) : IllegalStateException(message)

/**
 * Process-owned JsSIP engine. Every command is addressed to one immutable leg
 * token and is acknowledged by JavaScript before a Telecom callback completes.
 */
class VoiceWebViewClient(
    context: Context,
    private val identity: ClientIdentity,
    private val onEvent: (VoiceEngineEvent) -> Unit,
    private val onDiagnostic: (payload: String) -> Unit,
) : VoiceEngineControl {
    private val applicationContext = context.applicationContext
    private val mainHandler = Handler(Looper.getMainLooper())
    private val assetLoader = WebViewAssetLoader.Builder()
        .addPathHandler("/assets/", WebViewAssetLoader.AssetsPathHandler(applicationContext))
        .build()
    private var pageReady = false
    private var pendingProvisioning: VoiceProvisioning? = null
    private var activeProvisioning: VoiceProvisioning? = null
    private var currentTraceId = ""
    private val qualityLock = Any()
    private var pendingQualityEvent: VoiceEngineEvent? = null
    private var qualityDispatchPosted = false

    @Suppress("SetJavaScriptEnabled")
    val view: WebView = WebView(applicationContext).apply {
        settings.javaScriptEnabled = true
        settings.domStorageEnabled = false
        settings.mediaPlaybackRequiresUserGesture = false
        settings.mixedContentMode = WebSettings.MIXED_CONTENT_ALWAYS_ALLOW
        addJavascriptInterface(Bridge(), "VitiAndroid")
        webViewClient = object : WebViewClient() {
            override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? =
                assetLoader.shouldInterceptRequest(request.url)

            override fun onPageFinished(view: WebView, url: String) {
                pageReady = true
                recordNative("webview.page-finished", JSONObject().put("url", url))
                pendingProvisioning?.let(::startJavascript)
            }
        }
        webChromeClient = object : WebChromeClient() {
            override fun onConsoleMessage(message: ConsoleMessage): Boolean {
                recordNative(
                    "webview.console",
                    JSONObject()
                        .put("message", message.message())
                        .put("source_id", message.sourceId())
                        .put("line_number", message.lineNumber())
                        .put("level", message.messageLevel().name),
                )
                return super.onConsoleMessage(message)
            }

            override fun onPermissionRequest(request: PermissionRequest) {
                mainHandler.post {
                    val canRecord = hasMicrophonePermission()
                    val audioOnly = request.resources.filter { it == PermissionRequest.RESOURCE_AUDIO_CAPTURE }.toTypedArray()
                    val granted = canRecord && audioOnly.isNotEmpty()
                    recordNative(
                        "webview.permission-decided",
                        JSONObject().put("resources", request.resources.joinToString()).put("granted", granted),
                    )
                    if (granted) request.grant(audioOnly) else request.deny()
                }
            }
        }
        loadUrl(VOICE_PAGE)
    }

    override fun hasMicrophonePermission(): Boolean =
        ContextCompat.checkSelfPermission(applicationContext, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED

    /** Avoids tearing down an active leg when a recreated Activity repeats provisioning. */
    override fun start(provisioning: VoiceProvisioning) {
        if (provisioning == activeProvisioning || provisioning == pendingProvisioning) return
        pendingProvisioning = provisioning
        recordNative("voice.start-requested", JSONObject().put("provisioning", provisioning.toString()))
        if (pageReady) startJavascript(provisioning)
    }

    override suspend fun call(legToken: String, number: String, traceId: String) =
        command("call", legToken, JSONObject().put("number", number).put("trace_id", traceId))

    override suspend fun answer(legToken: String) = command("answer", legToken)
    override suspend fun reject(legToken: String) = command("reject", legToken)
    override suspend fun hangup(legToken: String) = command("hangup", legToken)
    override suspend fun setMuted(legToken: String, muted: Boolean) =
        command("mute", legToken, JSONObject().put("muted", muted))

    override suspend fun setTelecomMuted(legToken: String, muted: Boolean) =
        command("telecom-muted", legToken, JSONObject().put("muted", muted))

    override suspend fun setSystemInactive(legToken: String, inactive: Boolean) =
        command("inactive", legToken, JSONObject().put("inactive", inactive))

    override suspend fun sendDtmf(legToken: String, tone: Char) =
        command("dtmf", legToken, JSONObject().put("tone", tone.toString()))

    /** The hidden view is retained by the call service, not by an Activity. */
    fun detachFromUi() {
        (view.parent as? ViewGroup)?.removeView(view)
    }

    override fun destroy() {
        recordNative("voice.destroy-requested")
        mainHandler.post {
            view.removeJavascriptInterface("VitiAndroid")
            (view.parent as? ViewGroup)?.removeView(view)
            view.destroy()
        }
    }

    private fun startJavascript(provisioning: VoiceProvisioning) {
        pendingProvisioning = null
        activeProvisioning = provisioning
        val payload = JSONObject()
            .put("enabled", provisioning.enabled)
            .put("websocket_url", provisioning.websocketUrl)
            .put("domain", provisioning.domain)
            .put("username", provisioning.username)
            .put("password", provisioning.password)
            .put("diagnostics_enabled", provisioning.diagnosticsEnabled)
            .put("ice_servers", provisioning.iceServers.fold(org.json.JSONArray()) { array, server ->
                array.put(
                    JSONObject()
                        .put("urls", org.json.JSONArray(server.urls))
                        .put("username", server.username)
                        .put("credential", server.credential),
                )
            })
            .toString()
        val identityPayload = JSONObject().put("clientId", identity.id).put("clientName", identity.name).toString()
        evaluate("window.vitiVoiceStart(${JSONObject.quote(payload)},${JSONObject.quote(identityPayload)})")
    }

    private suspend fun command(action: String, legToken: String, args: JSONObject = JSONObject()) {
        require(legToken.isNotBlank()) { "A voice leg token is required" }
        val payload = JSONObject().put("action", action).put("leg_token", legToken).put("args", args).toString()
        val script = "window.vitiVoiceCommand(${JSONObject.quote(payload)})"
        recordNative("voice.command-requested", JSONObject().put("action", action).put("leg_token", legToken))
        val raw = withTimeout(COMMAND_TIMEOUT_MS) { evaluateAwait(script) }
        val encoded = runCatching { JSONTokener(raw).nextValue() as? String }.getOrNull()
            ?: throw VoiceCommandException("The voice engine returned an invalid acknowledgement")
        val response = runCatching { JSONObject(encoded) }.getOrElse {
            throw VoiceCommandException("The voice engine returned an invalid acknowledgement")
        }
        if (!response.optBoolean("ok")) {
            throw VoiceCommandException(response.optString("error", "The voice command failed"))
        }
        if (response.optString("leg_token") != legToken) {
            throw VoiceCommandException("The voice engine acknowledged a different call leg")
        }
        recordNative("voice.command-acknowledged", JSONObject().put("action", action).put("leg_token", legToken))
    }

    private suspend fun evaluateAwait(script: String): String = withContext(Dispatchers.Main.immediate) {
        suspendCancellableCoroutine { continuation ->
            if (!pageReady) {
                continuation.resumeWithException(VoiceCommandException("The voice engine is not ready"))
                return@suspendCancellableCoroutine
            }
            view.evaluateJavascript(script) { result ->
                if (continuation.isActive) continuation.resume(result)
            }
        }
    }

    private fun evaluate(script: String) {
        mainHandler.post {
            recordNative("webview.evaluate-requested", JSONObject().put("script", script))
            view.evaluateJavascript(script) { result ->
                recordNative("webview.evaluate-completed", JSONObject().put("script", script).put("result", result))
            }
        }
    }

    private fun recordNative(kind: String, data: JSONObject = JSONObject()) {
        val now = System.currentTimeMillis()
        onDiagnostic(
            JSONObject()
                .put("at_ms", now)
                .put("at_ns", now * 1_000_000)
                .put("source", "android-native")
                .put("kind", kind)
                .put("trace_id", currentTraceId)
                .put("data", data)
                .toString(),
        )
    }

    private inner class Bridge {
        @JavascriptInterface
        fun onVoiceEvent(payload: String) {
            val body = runCatching { JSONObject(payload) }.getOrNull() ?: return
            val type = body.optString("type")
            body.optString("trace_id").takeIf(String::isNotBlank)?.let { currentTraceId = it }
            if (type == "diagnostic") {
                onDiagnostic(body.toString())
                return
            }
            val cause = when (type) {
                "media" -> "sent=${body.optBoolean("sent")},received=${body.optBoolean("received")}"
                "mute-changed" -> "muted=${body.optBoolean("muted")}"
                else -> body.optString("cause")
            }
            val event = VoiceEngineEvent(
                type = type,
                peer = body.optString("from", body.optString("to")),
                cause = cause,
                traceId = body.optString("trace_id"),
                participantId = body.optString("participant_id"),
                legToken = body.optString("leg_token"),
                quality = VoiceQualityJson.parse(body.optJSONObject("quality")),
            )
            if (type == "quality") dispatchLatestQuality(event) else mainHandler.post { onEvent(event) }
        }
    }

    private fun dispatchLatestQuality(event: VoiceEngineEvent) {
        val shouldPost = synchronized(qualityLock) {
            pendingQualityEvent = event
            if (qualityDispatchPosted) false else {
                qualityDispatchPosted = true
                true
            }
        }
        if (!shouldPost) return
        mainHandler.post {
            val latest = synchronized(qualityLock) {
                qualityDispatchPosted = false
                pendingQualityEvent.also { pendingQualityEvent = null }
            }
            latest?.let(onEvent)
        }
    }

    companion object {
        private const val VOICE_PAGE = "https://appassets.androidplatform.net/assets/android-voice.html"
        private const val COMMAND_TIMEOUT_MS = 4_000L
    }
}
