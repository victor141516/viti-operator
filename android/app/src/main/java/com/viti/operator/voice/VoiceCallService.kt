package com.viti.operator.voice

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.app.ActivityManager
import android.content.Intent
import android.content.pm.ServiceInfo
import android.media.AudioAttributes
import android.media.RingtoneManager
import android.net.Uri
import android.os.Binder
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.os.SystemClock
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.app.Person
import com.viti.operator.IncomingCallRinger
import com.viti.operator.IncomingCallActivity
import com.viti.operator.MainActivity
import com.viti.operator.R
import com.viti.operator.data.SettingsStore
import com.viti.operator.data.VitiApi
import com.viti.operator.data.HistoryRepository
import com.viti.operator.IncomingMessageNotifier
import com.viti.operator.data.vitiClientIdentity
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch
import org.json.JSONObject
import java.security.MessageDigest
import java.util.UUID

/**
 * Process owner for the SIP engine and the one local Android call leg.
 * Activity recreation only subscribes to snapshots; it never owns the voice
 * engine, network transport, media or Telecom scope.
 */
class VoiceCallService : Service() {
    inner class LocalBinder : Binder() {
        val service: VoiceCallService get() = this@VoiceCallService
    }

    private val binder = LocalBinder()
    private val serviceScope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val settingsStore by lazy { SettingsStore(applicationContext) }
    private val history by lazy { HistoryRepository.get(applicationContext) }
    private val notificationManager by lazy { getSystemService(NotificationManager::class.java) }
    private val ringer by lazy { IncomingCallRinger(applicationContext, RINGTONE_CHANNEL_ID) }
    private val callRingtone by lazy {
        // Give ColorOS time to apply the soundless CallStyle notification before
        // opening our audio stream. The token-fenced delay needs no Activity.
        IncomingCallRingtoneController(ringer::start, ringer::stop, serviceScope, 600L)
    }
    private val signalingWakeLock by lazy {
        getSystemService(PowerManager::class.java)
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "$packageName:voice-signaling")
            .apply { setReferenceCounted(false) }
    }
    private val serverConnectionAlert by lazy { ServerConnectionAlert(applicationContext) }
    private val serverApiReachable = MutableStateFlow<Boolean?>(null)
    private val carrierAvailability = MutableStateFlow(CarrierAvailability.UNKNOWN)
    private var lastCarrierStatus: com.viti.operator.data.RuntimeStatus? = null
    private val registrationLease = VoiceRegistrationLease()
    private val signalingProbe = VoiceSignalingProbe(SystemClock::elapsedRealtime)
    private val probeSession = UUID.randomUUID().toString()
    private val probeClientHash by lazy {
        MessageDigest.getInstance("SHA-256").digest(vitiClientIdentity().id.toByteArray(Charsets.UTF_8))
            .joinToString("") { "%02x".format(it) }
    }
    private val claimedParticipants = mutableSetOf<String>()
    private val callActionIntents = mutableSetOf<PendingIntent>()
    private var callActionToken = ""

    lateinit var runtime: VoiceCallRuntime
        private set
    lateinit var voiceClient: NativeVoiceClient
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
        val messageNotifier = IncomingMessageNotifier(applicationContext)
        history.start(serviceScope, settingsStore.settings, messageNotifier::show)
        serviceScope.launch {
            // Presence is independent of UI/history refresh and remains compact.
            while (true) {
                runCatching { VitiApi(settingsStore.settings.first()).heartbeat(identity) }
                kotlinx.coroutines.delay(20_000)
            }
        }
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
        val client = NativeVoiceClient(
            applicationContext,
            identity,
            serviceScope,
            onEvent = ::onVoiceEvent,
        )
        voiceClient = client
        runtimeReference = VoiceCallRuntime(
            scope = serviceScope,
            voiceClient = client,
            telecomGateway = telecom,
            legacyAudio = legacy,
			callSessionControl = object : VoiceCallSessionControl {
				override suspend fun isCarrierCallActive(traceId: String): Boolean {
					val settings = settingsStore.settings.first()
					return VitiApi(settings).isCarrierCallActive(traceId)
				}

				override suspend fun leave(traceId: String, connectionId: String, previousConnectionId: String, operationId: String) {
					val settings = settingsStore.settings.first()
					VitiApi(settings).leaveCallParticipant(traceId, identity, connectionId, previousConnectionId, operationId)
				}
			},
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
        serviceScope.launchVoiceSignalingSupervisor(
            maintain = {
                signalingProbe.tick()
                // Bound the lease so a stalled supervisor cannot leak a lock.
                if (voiceClient.signalingEnabled) signalingWakeLock.acquire(SIGNALING_WAKE_LEASE_MS)
                else if (signalingWakeLock.isHeld) signalingWakeLock.release()
                voiceClient.maintainRegistration()
                if (registrationLease.hasExpired()) {
                    runtime.onVoiceEvent(VoiceEngineEvent("unregistered", cause = "Voice registration expired"))
                }
                recordSignalingSnapshot()
            },
            onFailure = { error ->
                signalingProbe.observe("maintenance-failed")
                recordSignalingSnapshot()
                Log.w(TAG, "Voice signaling check failed; retrying", error)
                // Readiness follows the confirmed server lease, not a UI tick.
                if (!registrationLease.isValid()) {
                    runtime.onVoiceEvent(VoiceEngineEvent("unregistered", cause = "Voice signaling is not responding"))
                }
            },
            release = { if (signalingWakeLock.isHeld) signalingWakeLock.release() },
        )
        serviceScope.launchVoiceProvisioningSupervisor(
            settings = settingsStore.settings,
            load = { settings -> VitiApi(settings).voice() },
            apply = runtime::start,
            onFailure = { error -> Log.w(TAG, "Could not refresh voice provisioning; retrying", error) },
            onReachabilityChanged = ::onServerApiReachabilityChanged,
            refreshDelayMs = SERVER_PROBE_INTERVAL_MS,
        )
        serviceScope.launchCarrierStatusSupervisor(
            settings = settingsStore.settings,
            load = { settings -> VitiApi(settings).status().also {
                if (lastCarrierStatus != it) { lastCarrierStatus = it; history.notifyMetadataChanged() }
            } },
            onState = {
                if (carrierAvailability.value != it) history.notifyMetadataChanged()
                carrierAvailability.value = it
            },
            pollIntervalMs = SERVER_PROBE_INTERVAL_MS,
        )
        val availability = combine(
            runtime.snapshot.map { it.serverConnectionAvailable },
            serverApiReachable,
            carrierAvailability,
            ::callAvailability,
        )
        serviceScope.launch {
            availability.collect { state ->
                val idle = runtime.snapshot.value.legToken.isBlank()
                if (idle) showIdleForegroundNotification(state)
                if (serverConnectionAlert.visible) {
                    if (!state.needsLocalConnectionAlert()) serverConnectionAlert.dismiss()
                    else serverConnectionAlert.show(state, ring = idle)
                }
            }
        }
        serviceScope.launchServerConnectionAlertMonitor(
            connectionAvailable = availability.map { !it.needsLocalConnectionAlert() },
            showAlert = {
                val state = currentAvailability()
                Log.w(TAG, "Call availability: $state; showing persistent connection alert")
                serverConnectionAlert.show(state, ring = runtime.snapshot.value.legToken.isBlank())
            },
            dismissAlert = {
                if (serverConnectionAlert.visible) Log.i(TAG, "Local server connection restored; dismissing connection alert")
                serverConnectionAlert.dismiss()
            },
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
            ACTION_SILENCE_CONNECTION_ALERT -> serverConnectionAlert.silence(
                intent?.getStringExtra(EXTRA_CONNECTION_ALERT_TOKEN),
            )
        }
        return START_STICKY
    }

    override fun onDestroy() {
        history.stop()
        callRingtone.close()
        serverConnectionAlert.dismiss()
        invalidateCallActions("")
        notificationManager.cancel(CALL_NOTIFICATION_ID)
        notificationManager.cancel(SERVICE_NOTIFICATION_ID)
        runtime.close()
        serviceScope.cancel()
        if (signalingWakeLock.isHeld) signalingWakeLock.release()
        super.onDestroy()
    }

    private fun onVoiceEvent(event: VoiceEngineEvent) {
        signalingProbe.observe(event.type, event.cause)
        when (event.type) {
            "registered" -> registrationLease.confirmed(event.registrationExpiresAtMs)
            "disconnected", "unregistered", "registration-failed" -> registrationLease.lost()
        }
        runtime.onVoiceEvent(event)
        if (event.type in setOf("registered", "unregistered", "registration-failed", "disconnected")) recordSignalingSnapshot()
        if (event.type == "active" && event.traceId.isNotBlank() && event.participantId.isNotBlank()) {
            val key = "${event.traceId}:${event.participantId}"
            if (claimedParticipants.add(key)) {
                serviceScope.launch {
                    var completed = false
                    try {
                        while (runtime.snapshot.value.legToken == event.legToken && runtime.snapshot.value.phase != VoiceCallPhase.ENDING) {
                            val settings = settingsStore.settings.first()
                            val result = runCatching {
                                VitiApi(settings).claimRecordingParticipant(
                                    event.traceId,
                                    event.participantId,
                                    vitiClientIdentity(),
                                )
                            }
                            if (result.isSuccess) {
                                completed = true
                                return@launch
                            }
                            recordDiagnostic(
                                JSONObject()
                                    .put("at_ms", System.currentTimeMillis())
                                    .put("source", "android-call-service")
                                    .put("kind", "telecom.participant-claim-retry")
                                    .put("data", JSONObject()
                                        .put("trace_id", event.traceId)
                                        .put("participant_id", event.participantId)
                                        .put("error", result.exceptionOrNull().toString()))
                                    .toString(),
                            )
                            kotlinx.coroutines.delay(1_500)
                        }
                    } finally {
                        if (!completed) claimedParticipants.remove(key)
                    }
                }
            }
        }
    }

    private fun onSnapshot(snapshot: VoiceCallSnapshot) {
        val availability = callAvailability(
            snapshot.serverConnectionAvailable,
            serverApiReachable.value,
            carrierAvailability.value,
        )
        if (snapshot.legToken.isBlank()) {
			claimedParticipants.clear()
            callRingtone.update(snapshot)
            if (serverConnectionAlert.visible && availability.needsLocalConnectionAlert()) {
                serverConnectionAlert.startRinging()
            }
            invalidateCallActions("")
            showIdleForegroundNotification(availability)
            notificationManager.cancel(CALL_NOTIFICATION_ID)
            return
        }
        serverConnectionAlert.pauseRinging()
        showCallForeground(snapshot)
        callRingtone.update(snapshot)
    }

    private fun onServerApiReachabilityChanged(reachable: Boolean) {
        if (serverApiReachable.value == reachable) return
        serverApiReachable.value = reachable
        val current = runtime.snapshot.value
        if (current.legToken.isBlank()) {
            showIdleForegroundNotification(
                currentAvailability(),
            )
        }
    }

    private fun currentAvailability() = callAvailability(
        runtime.snapshot.value.serverConnectionAvailable,
        serverApiReachable.value,
        carrierAvailability.value,
    )

    private fun showCallForeground(snapshot: VoiceCallSnapshot) {
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
        notificationManager.cancel(SERVICE_NOTIFICATION_ID)
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
        val builder = callNotificationBuilder(this, incoming)
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
            .setOngoing(true)
            .setContentIntent(incomingScreen ?: openApp)
            .apply {
                // Always attach the sanctioned call FSI. Android decides whether
                // to launch it or show its persistent/expanded heads-up fallback.
                if (incomingScreen != null) setFullScreenIntent(incomingScreen, true)
            }
        return builder.build()
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

    private fun showIdleForegroundNotification(availability: CallAvailability = CallAvailability.CONNECTING) {
        val openApp = PendingIntent.getActivity(
            this,
            OPEN_REQUEST,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(this, SERVICE_CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_foreground)
            .setContentTitle("Viti Operator")
            .setContentText(availability.detail)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setOngoing(true)
            .setSilent(true)
            .setContentIntent(openApp)
            .build()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(
                SERVICE_NOTIFICATION_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE,
            )
        } else if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(SERVICE_NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_NONE)
        } else {
            startForeground(SERVICE_NOTIFICATION_ID, notification)
        }
    }

    private fun recordSignalingSnapshot() {
        val socketStats = voiceClient.nativeSipSocket.stats()
        val process = ActivityManager.RunningAppProcessInfo()
        ActivityManager.getMyMemoryState(process)
        val data = JSONObject()
            .put("client_hash", probeClientHash).put("session", probeSession)
            .put("build", VoiceSignalingProbe.BUILD)
            .put("socket_sent", socketStats.sent).put("socket_received", socketStats.received)
            .put("socket_state", socketStats.state)
            .put("tick_count", signalingProbe.tickCount)
            .put("dispatch_count", voiceClient.maintenanceDispatchCount)
            .put("uptime_ms", signalingProbe.uptimeMs())
            .put("event_age_ms", signalingProbe.eventAgeMs())
            .put("event", signalingProbe.event)
            .put("lease_remaining_ms", registrationLease.remainingMs())
            .put("lease_valid", registrationLease.isValid())
            .put("enabled", voiceClient.signalingEnabled)
            .put("webview_owned", false)
            .put("process_importance", process.importance)
            .put("wake_lock", signalingWakeLock.isHeld)
            .put("voice_registered", runtime.snapshot.value.serverConnectionAvailable)
        serverApiReachable.value?.let { data.put("api_reachable", it) }
        recordDiagnostic(JSONObject().put("source", "android-signaling")
            .put("kind", "signaling.snapshot").put("data", data).toString())
    }

    private fun recordDiagnostic(payload: String) {
        val kind = runCatching { JSONObject(payload).optString("kind") }.getOrDefault("")
        if (kind.startsWith("telecom.")) Log.d(TAG, payload)
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
            // Preserve the existing user's ringtone and mute preferences.
            NotificationChannel(RINGTONE_CHANNEL_ID, "Call ringtone", NotificationManager.IMPORTANCE_HIGH).apply {
                description = "Sound preferences for the service-owned call ringtone"
                setSound(sound, attributes)
                enableVibration(true)
                lockscreenVisibility = Notification.VISIBILITY_PUBLIC
            },
        )
        notificationManager.createNotificationChannel(
            NotificationChannel(CALL_CHANNEL_ID, "Incoming call alerts",
                notificationManager.getNotificationChannel(RINGTONE_CHANNEL_ID)?.importance
                    ?: NotificationManager.IMPORTANCE_HIGH).apply {
                description = "Floating and lock-screen incoming call controls; Viti plays the ringtone separately"
                setSound(null, null)
                enableVibration(notificationManager.getNotificationChannel(RINGTONE_CHANNEL_ID)?.shouldVibrate() ?: true)
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
        const val ACTION_SILENCE_CONNECTION_ALERT = "com.viti.operator.voice.action.SILENCE_CONNECTION_ALERT"
        const val EXTRA_CONNECTION_ALERT_TOKEN = "com.viti.operator.voice.extra.CONNECTION_ALERT_TOKEN"
        const val EXTRA_LEG_TOKEN = "com.viti.operator.voice.extra.LEG_TOKEN"
        const val EXTRA_CALLER_NAME = "com.viti.operator.voice.extra.CALLER_NAME"
        const val CALL_CHANNEL_ID = "incoming_call_alerts_v1"
        const val RINGTONE_CHANNEL_ID = "incoming_calls_v4"
        private const val SERVICE_CHANNEL_ID = "call_service_v1"
        private const val CALL_NOTIFICATION_ID = 4402
        private const val SERVICE_NOTIFICATION_ID = 4407
        private const val OPEN_REQUEST = 4401
        private const val ANSWER_REQUEST = 4403
        private const val REJECT_REQUEST = 4404
        private const val HANGUP_REQUEST = 4405
        private const val INCOMING_SCREEN_REQUEST = 4406
        private const val SIGNALING_WAKE_LEASE_MS = 120_000L
        private const val SERVER_PROBE_INTERVAL_MS = 10_000L
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
