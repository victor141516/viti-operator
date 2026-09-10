package com.viti.operator.voice

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.media.AudioAttributes
import android.media.RingtoneManager
import android.net.Uri
import android.os.Binder
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.app.Person
import com.viti.operator.IncomingCallRinger
import com.viti.operator.IncomingCallActivity
import com.viti.operator.MainActivity
import com.viti.operator.R
import com.viti.operator.data.SettingsStore
import com.viti.operator.data.VitiApi
import com.viti.operator.data.vitiClientIdentity
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import org.json.JSONObject

/**
 * Process owner for the SIP engine and the one local Android call leg.
 * Activity recreation only detaches and reattaches the hidden WebView; it never
 * destroys the call or its Telecom scope.
 */
class VoiceCallService : Service() {
    inner class LocalBinder : Binder() {
        val service: VoiceCallService get() = this@VoiceCallService
    }

    private val binder = LocalBinder()
    private val serviceScope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val settingsStore by lazy { SettingsStore(applicationContext) }
    private val notificationManager by lazy { getSystemService(NotificationManager::class.java) }
    private val ringer by lazy { IncomingCallRinger(applicationContext, CALL_CHANNEL_ID) }
    private val claimedParticipants = mutableSetOf<String>()
    private val callActionIntents = mutableSetOf<PendingIntent>()
    private var callActionToken = ""

    lateinit var runtime: VoiceCallRuntime
        private set
    lateinit var voiceClient: VoiceWebViewClient
        private set
    var coreTelecomAvailable: Boolean = false
        private set
    var startedForBackgroundCalls: Boolean = false
        private set
    val snapshot get() = runtime.snapshot

    override fun onCreate() {
        super.onCreate()
        createCallChannel()
        createServiceChannel()
        showIdleForegroundNotification()
        val identity = vitiClientIdentity()
        lateinit var runtimeReference: VoiceCallRuntime
        coreTelecomAvailable =
            Build.VERSION.SDK_INT >= Build.VERSION_CODES.O &&
            supportsCoreTelecom(
                Build.VERSION.SDK_INT,
                packageManager.hasSystemFeature(TELECOM_FEATURE),
            )
        // Keep the API check at the construction site so both the runtime and
        // static analysis can prove this API-26 class is unreachable on 24-25.
        val telecom = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O && coreTelecomAvailable) {
            CoreTelecomGateway(applicationContext)
        } else {
            null
        }
        val legacy: LegacyAudioControl = if (telecom == null) {
            LegacyCallAudioController(
                applicationContext,
                onInactiveChanged = { inactive -> runtimeReference.onLegacyInactiveChanged(inactive) },
                onAudioState = { state -> runtimeReference.onLegacyAudioState(state) },
                onAnswer = { runtimeReference.answer() },
                onHangup = {
                    val snapshot = runtimeReference.snapshot.value
                    if (snapshot.direction == VoiceCallDirection.INCOMING && snapshot.phase == VoiceCallPhase.RINGING) {
                        runtimeReference.reject(snapshot.legToken)
                    } else {
                        runtimeReference.hangup(snapshot.legToken)
                    }
                },
            )
        } else {
            NoOpLegacyAudioControl
        }
        val client = VoiceWebViewClient(
            applicationContext,
            identity,
            onEvent = ::onVoiceEvent,
            onDiagnostic = ::recordDiagnostic,
        )
        voiceClient = client
        runtimeReference = VoiceCallRuntime(
            scope = serviceScope,
            voiceClient = client,
            telecomGateway = telecom,
            legacyAudio = legacy,
            onSnapshot = ::onSnapshot,
            onDiagnostic = { kind, data ->
                recordDiagnostic(
                    JSONObject()
                        .put("at_ms", System.currentTimeMillis())
                        .put("source", "android-call-service")
                        .put("kind", kind)
                        .put("data", JSONObject(data))
                        .toString(),
                )
            },
        )
        runtime = runtimeReference
        serviceScope.launchVoiceProvisioningSupervisor(
            settings = settingsStore.settings,
            load = { settings -> VitiApi(settings).voice() },
            apply = runtime::start,
            onFailure = { error -> Log.w(TAG, "Could not refresh voice provisioning; retrying", error) },
        )
    }

    override fun onBind(intent: Intent?): IBinder = binder

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startedForBackgroundCalls = true
        val token = intent?.getStringExtra(EXTRA_LEG_TOKEN)?.takeIf(String::isNotBlank)
        when (intent?.action) {
            ACTION_ANSWER -> runtime.answer(token)
            ACTION_REJECT -> runtime.reject(token)
            ACTION_HANGUP -> runtime.hangup(token)
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        ringer.stop()
        invalidateCallActions("")
        notificationManager.cancel(CALL_NOTIFICATION_ID)
        runtime.close()
        serviceScope.cancel()
        super.onDestroy()
    }

    fun detachVoiceView() = voiceClient.detachFromUi()

    private fun onVoiceEvent(event: VoiceEngineEvent) {
        runtime.onVoiceEvent(event)
        if (event.type == "active" && event.traceId.isNotBlank() && event.participantId.isNotBlank()) {
            val key = "${event.traceId}:${event.participantId}"
            if (claimedParticipants.add(key)) {
                serviceScope.launch {
                    val settings = settingsStore.settings.first()
                    runCatching {
                        VitiApi(settings).claimRecordingParticipant(
                            event.traceId,
                            event.participantId,
                            vitiClientIdentity(),
                        )
                    }
                }
            }
        }
    }

    private fun onSnapshot(snapshot: VoiceCallSnapshot) {
        if (snapshot.legToken.isBlank()) {
            ringer.stop()
            invalidateCallActions("")
            showIdleForegroundNotification()
            return
        }
        if (snapshot.direction == VoiceCallDirection.INCOMING && snapshot.phase == VoiceCallPhase.RINGING) {
            ringer.start()
        } else {
            ringer.stop()
        }
        val notification = callNotification(snapshot)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(
                CALL_NOTIFICATION_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_PHONE_CALL or ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE,
            )
        } else if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(CALL_NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_PHONE_CALL)
        } else {
            startForeground(CALL_NOTIFICATION_ID, notification)
        }
    }

    private fun callNotification(snapshot: VoiceCallSnapshot): Notification {
        val token = snapshot.legToken
        invalidateCallActions(token)
        val peer = snapshot.displayName.ifBlank { snapshot.peer }.ifBlank { "Private number" }
        val person = Person.Builder().setName(peer).setImportant(true).build()
        val openApp = PendingIntent.getActivity(
            this,
            OPEN_REQUEST,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val reject = serviceAction(ACTION_REJECT, token, REJECT_REQUEST)
        val hangup = serviceAction(ACTION_HANGUP, token, HANGUP_REQUEST)
        val endCall = if (shouldRejectFromNotification(snapshot)) reject else hangup
        val answer = if (snapshot.microphonePermissionRequired) {
            incomingCallScreen(token, peer, requestAnswer = true)
        } else {
            serviceAction(ACTION_ANSWER, token, ANSWER_REQUEST)
        }
        val incoming = snapshot.direction == VoiceCallDirection.INCOMING && snapshot.phase == VoiceCallPhase.RINGING
        val incomingScreen = if (incoming) incomingCallScreen(token, peer) else null
        return NotificationCompat.Builder(this, CALL_CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle(if (incoming) "Incoming service call" else peer)
            .setContentText(snapshot.voice.detail)
            .setStyle(
                if (incoming) NotificationCompat.CallStyle.forIncomingCall(person, reject, answer)
                else NotificationCompat.CallStyle.forOngoingCall(person, endCall),
            )
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setPriority(NotificationCompat.PRIORITY_MAX)
            .setVisibility(NotificationCompat.VISIBILITY_PUBLIC)
            .setSilent(true)
            .setOngoing(true)
            .setContentIntent(incomingScreen ?: openApp)
            .apply {
                // Always attach the sanctioned call FSI. Android decides whether
                // to launch it or show its persistent/expanded heads-up fallback.
                if (incomingScreen != null) setFullScreenIntent(incomingScreen, true)
            }
            .build()
    }

    private fun incomingCallScreen(
        token: String,
        peer: String,
        requestAnswer: Boolean = false,
    ) = PendingIntent.getActivity(
        this,
        if (requestAnswer) ANSWER_REQUEST else INCOMING_SCREEN_REQUEST,
        Intent(this, IncomingCallActivity::class.java)
            .setAction(if (requestAnswer) IncomingCallActivity.ACTION_ANSWER_CALL else ACTION_OPEN_INCOMING)
            .setData(Uri.parse(callActionData(if (requestAnswer) IncomingCallActivity.ACTION_ANSWER_CALL else ACTION_OPEN_INCOMING, token)))
            .putExtra(EXTRA_LEG_TOKEN, token)
            .putExtra(EXTRA_CALLER_NAME, peer)
            .addFlags(
                Intent.FLAG_ACTIVITY_NEW_TASK or
                    Intent.FLAG_ACTIVITY_CLEAR_TOP or
                    Intent.FLAG_ACTIVITY_SINGLE_TOP,
            ),
        PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
    ).also(callActionIntents::add)

    private fun serviceAction(action: String, token: String, requestCode: Int) = PendingIntent.getService(
        this,
        requestCode,
        Intent(this, VoiceCallService::class.java)
            .setAction(action)
            .setData(Uri.parse(callActionData(action, token)))
            .putExtra(EXTRA_LEG_TOKEN, token),
        PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
    ).also(callActionIntents::add)

    private fun invalidateCallActions(nextToken: String) {
        if (callActionToken == nextToken) return
        callActionIntents.forEach(PendingIntent::cancel)
        callActionIntents.clear()
        callActionToken = nextToken
    }

    private fun showIdleForegroundNotification() {
        val openApp = PendingIntent.getActivity(
            this,
            OPEN_REQUEST,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(this, SERVICE_CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle("Viti Operator")
            .setContentText("Service calls are ready")
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setOngoing(true)
            .setSilent(true)
            .setContentIntent(openApp)
            .build()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(
                CALL_NOTIFICATION_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE,
            )
        } else if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(CALL_NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_NONE)
        } else {
            startForeground(CALL_NOTIFICATION_ID, notification)
        }
    }

    private fun recordDiagnostic(payload: String) {
        serviceScope.launch {
            runCatching { VitiApi(settingsStore.settings.first()).recordDiagnostic(JSONObject(payload)) }
        }
    }

    private fun createCallChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val sound = RingtoneManager.getDefaultUri(RingtoneManager.TYPE_RINGTONE)
        val attributes = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_NOTIFICATION_RINGTONE)
            .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
            .build()
        notificationManager.createNotificationChannel(
            NotificationChannel(CALL_CHANNEL_ID, "Incoming calls", NotificationManager.IMPORTANCE_HIGH).apply {
                description = "Alerts for calls received through Viti Operator"
                setSound(sound, attributes)
                enableVibration(true)
                lockscreenVisibility = Notification.VISIBILITY_PUBLIC
            },
        )
    }

    private fun createServiceChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        notificationManager.createNotificationChannel(
            NotificationChannel(SERVICE_CHANNEL_ID, "Call service", NotificationManager.IMPORTANCE_LOW).apply {
                description = "Keeps Viti service calls available in the background"
                setSound(null, null)
                enableVibration(false)
                setShowBadge(false)
                lockscreenVisibility = Notification.VISIBILITY_PRIVATE
            },
        )
    }

    companion object {
        const val ACTION_ANSWER = "com.viti.operator.voice.action.ANSWER"
        const val ACTION_REJECT = "com.viti.operator.voice.action.REJECT"
        const val ACTION_HANGUP = "com.viti.operator.voice.action.HANGUP"
        const val ACTION_OPEN_INCOMING = "com.viti.operator.voice.action.OPEN_INCOMING"
        const val EXTRA_LEG_TOKEN = "com.viti.operator.voice.extra.LEG_TOKEN"
        const val EXTRA_CALLER_NAME = "com.viti.operator.voice.extra.CALLER_NAME"
        const val CALL_CHANNEL_ID = "incoming_calls_v2"
        private const val SERVICE_CHANNEL_ID = "call_service_v1"
        private const val CALL_NOTIFICATION_ID = 4402
        private const val OPEN_REQUEST = 4401
        private const val ANSWER_REQUEST = 4403
        private const val REJECT_REQUEST = 4404
        private const val HANGUP_REQUEST = 4405
        private const val INCOMING_SCREEN_REQUEST = 4406
        private const val TAG = "VitiVoiceService"
    }
}

private object NoOpLegacyAudioControl : LegacyAudioControl {
    override fun begin(direction: VoiceCallDirection) = Unit
    override fun setActive(value: Boolean) = Unit
    override fun requestEndpoint(endpointId: String) = Unit
    override fun end() = Unit
    override fun close() = Unit
}
