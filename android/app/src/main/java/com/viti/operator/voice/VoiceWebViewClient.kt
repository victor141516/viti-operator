package com.viti.operator.voice

import android.Manifest
import android.app.Activity
import android.content.Context
import android.content.pm.PackageManager
import android.media.AudioManager
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.webkit.JavascriptInterface
import android.webkit.ConsoleMessage
import android.webkit.PermissionRequest
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.core.content.ContextCompat
import androidx.webkit.WebViewAssetLoader
import com.viti.operator.data.VoiceProvisioning
import com.viti.operator.data.ClientIdentity
import org.json.JSONObject
import java.util.UUID

class VoiceWebViewClient(
    private val activity: Activity,
    private val identity: ClientIdentity,
    private val onEvent: (type: String, peer: String, cause: String, traceId: String, participantId: String) -> Unit,
    private val onDiagnostic: (payload: String) -> Unit,
) {
    private val mainHandler = Handler(Looper.getMainLooper())
    private val audioManager = activity.getSystemService(Context.AUDIO_SERVICE) as AudioManager
    private val assetLoader = WebViewAssetLoader.Builder()
        .addPathHandler("/assets/", WebViewAssetLoader.AssetsPathHandler(activity))
        .build()
    private var pageReady = false
    private var pendingProvisioning: VoiceProvisioning? = null
    private var currentTraceId = ""

    @Suppress("SetJavaScriptEnabled")
    val view: WebView = WebView(activity).apply {
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
                    val canRecord = ContextCompat.checkSelfPermission(activity, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED
                    val audioOnly = request.resources.filter { it == PermissionRequest.RESOURCE_AUDIO_CAPTURE }.toTypedArray()
                    val granted = canRecord && audioOnly.isNotEmpty()
                    recordNative("webview.permission-decided", JSONObject().put("resources", request.resources.joinToString()).put("granted", granted))
                    if (granted) request.grant(audioOnly) else request.deny()
                }
            }
        }
        loadUrl(VOICE_PAGE)
    }

    fun start(provisioning: VoiceProvisioning) {
        pendingProvisioning = provisioning
        recordNative("voice.start-requested", JSONObject().put("provisioning", provisioning.toString()))
        if (pageReady) startJavascript(provisioning)
    }

    fun call(number: String) {
        currentTraceId = UUID.randomUUID().toString()
        recordNative("voice.call-requested", JSONObject().put("number", number))
        evaluate("window.vitiVoiceCall(${JSONObject.quote(number)},${JSONObject.quote(currentTraceId)})")
    }

    fun answer() { recordNative("voice.answer-requested"); evaluate("window.vitiVoiceAnswer()") }
    fun sendDtmf(tone: Char) {
        recordNative("voice.dtmf-requested", JSONObject().put("tone", tone.toString()))
        evaluate("window.vitiVoiceDtmf(${JSONObject.quote(tone.toString())})")
    }
    fun reject() { recordNative("voice.reject-requested"); evaluate("window.vitiVoiceReject()") }
    fun hangup() { recordNative("voice.hangup-requested"); evaluate("window.vitiVoiceHangup()") }

    fun destroy() {
        recordNative("voice.destroy-requested")
        evaluate("window.vitiVoiceHangup()")
        view.removeJavascriptInterface("VitiAndroid")
        view.destroy()
        audioManager.mode = AudioManager.MODE_NORMAL
    }

    private fun startJavascript(provisioning: VoiceProvisioning) {
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

    private fun evaluate(script: String) {
        recordNative("webview.evaluate-requested", JSONObject().put("script", script))
        view.evaluateJavascript(script) { result ->
            recordNative("webview.evaluate-completed", JSONObject().put("script", script).put("result", result))
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
            val peer = body.optString("from", body.optString("to"))
            val traceId = body.optString("trace_id")
            val participantId = body.optString("participant_id")
            val cause = if (type == "media") {
                "sent=${body.optBoolean("sent")},received=${body.optBoolean("received")}"
            } else {
                body.optString("cause")
            }
            mainHandler.post {
                // Do not switch to communication routing while an incoming call
                // is merely ringing: some devices then route or suppress the
                // ringtone as if call audio had already started.
                if (type in setOf("calling", "ringing", "active", "media")) {
                    audioManager.mode = AudioManager.MODE_IN_COMMUNICATION
                } else if (type in setOf("ended", "failed")) {
                    audioManager.mode = AudioManager.MODE_NORMAL
                }
                onEvent(type, peer, cause, traceId, participantId)
            }
        }
    }

    companion object {
        private const val VOICE_PAGE = "https://appassets.androidplatform.net/assets/android-voice.html"
    }
}
