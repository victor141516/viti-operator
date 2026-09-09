package com.viti.operator

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.media.AudioAttributes
import android.media.RingtoneManager
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.util.Log
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.core.content.ContextCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.Person
import androidx.lifecycle.lifecycleScope
import com.viti.operator.auth.RemoteAuthService
import com.viti.operator.auth.ShizukuSetup
import com.viti.operator.data.SettingsStore
import com.viti.operator.data.ContactsRepository
import com.viti.operator.data.PhoneRoutingSettings
import com.viti.operator.data.Message
import com.viti.operator.data.vitiClientIdentity
import com.viti.operator.ui.VitiApp
import com.viti.operator.ui.VitiTheme
import com.viti.operator.voice.VoiceWebViewClient
import kotlinx.coroutines.launch
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withContext
import org.json.JSONObject

class MainActivity : ComponentActivity() {
    private val viewModel by viewModels<MainViewModel> { MainViewModel.Factory(SettingsStore(applicationContext)) }
    private lateinit var voiceClient: VoiceWebViewClient
    private lateinit var incomingCallRinger: IncomingCallRinger
    private val clientIdentity by lazy { vitiClientIdentity() }
    private var pendingMicrophoneAction: (() -> Unit)? = null
    private val notificationPermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) { }
    private val microphonePermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        val action = pendingMicrophoneAction
        pendingMicrophoneAction = null
        if (granted) action?.invoke() else viewModel.showNotice("Microphone access is required for calls")
    }
    private val contactsPermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        if (granted) loadContacts() else viewModel.showNotice("Contacts permission was not granted")
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        applyDebugServerOverride(intent)
        publishClientIdentity()
        createNotificationChannels()
        lifecycleScope.launch {
            viewModel.incomingMessages.collect(::showIncomingMessageNotification)
        }
        voiceClient = VoiceWebViewClient(this, clientIdentity, ::onVoiceEvent, viewModel::recordVoiceDiagnostic)
        incomingCallRinger = IncomingCallRinger(applicationContext, CALL_CHANNEL_ID)
        setContent {
            VitiTheme {
                VitiApp(
                    viewModel = viewModel,
                    voiceClient = voiceClient,
                    onPlaceCall = ::placeCall,
                    onAnswerCall = ::answerCall,
                    onRejectCall = voiceClient::reject,
                    onHangupCall = voiceClient::hangup,
                    onSendDtmf = voiceClient::sendDtmf,
                    onPrepareShizuku = ::prepareShizuku,
                    onStartAuthAgent = ::startAuthAgent,
                    onStopAuthAgent = ::stopAuthAgent,
					onEnableServerLine = ::enableServerLine,
					onReturnLineToPhone = ::returnLineToPhone,
                    onRequestContacts = ::requestContacts,
                )
            }
        }
        handleNotificationIntent(intent)
        showDebugNotificationIfRequested(intent)
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    override fun onDestroy() {
        incomingCallRinger.stop()
        voiceClient.destroy()
        super.onDestroy()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        applyDebugServerOverride(intent)
        handleNotificationIntent(intent)
        showDebugNotificationIfRequested(intent)
    }

    /**
     * Deterministic configuration hook for physical-device integration tests.
     * Release builds ignore these extras completely; normal users configure
     * the same values through the Material 3 connection screen.
     */
    private fun applyDebugServerOverride(intent: Intent) {
        if (!BuildConfig.DEBUG) return
        val serverUrl = intent.getStringExtra(DEBUG_SERVER_URL)
            ?.trim()
            ?.trimEnd('/')
            ?.takeIf(String::isNotBlank)
            ?: return
        val apiKey = intent.getStringExtra(DEBUG_API_KEY)?.trim()
        lifecycleScope.launch {
            val store = SettingsStore(applicationContext)
            val current = store.settings.first()
            store.save(
                current.copy(
                    serverUrl = serverUrl,
                    apiKey = apiKey ?: current.apiKey,
                ),
            )
        }
    }

    override fun onStart() {
        super.onStart()
        publishClientIdentity()
        // A permission already granted in an earlier session must populate the
        // dialer immediately; otherwise reinstalling or reopening the activity
        // leaves a misleading "Use contacts" prompt until it is tapped.
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.READ_CONTACTS) == PackageManager.PERMISSION_GRANTED) {
            loadContacts()
        }
    }

    private fun publishClientIdentity() {
        viewModel.setClientIdentity(clientIdentity)
    }

    private fun placeCall(number: String) = withMicrophonePermission { voiceClient.call(number) }

    private fun answerCall() = withMicrophonePermission { voiceClient.answer() }

    private fun onVoiceEvent(type: String, peer: String, cause: String, traceId: String, participantId: String) {
        Log.i("VitiVoice", "event=$type")
        viewModel.onVoiceEvent(type, peer, cause, traceId, participantId)
        when (type) {
            "incoming" -> {
                showIncomingCallNotification(peer)
                incomingCallRinger.start()
            }
            "active", "media", "ended", "failed", "registration-failed" -> stopIncomingCallAlert()
        }
    }

    private fun handleNotificationIntent(intent: Intent) {
        when (intent.action) {
            ACTION_ANSWER_CALL -> {
                intent.action = null
                stopIncomingCallAlert()
                answerCall()
            }
            ACTION_REJECT_CALL -> {
                intent.action = null
                stopIncomingCallAlert()
                voiceClient.reject()
            }
        }
    }

    private fun showDebugNotificationIfRequested(intent: Intent) {
        if (!BuildConfig.DEBUG) return
        val caller = intent.getStringExtra(DEBUG_INCOMING_CALL)?.trim()?.takeIf(String::isNotBlank) ?: return
        intent.removeExtra(DEBUG_INCOMING_CALL)
        showIncomingCallNotification(caller)
        incomingCallRinger.start()
    }

    private fun stopIncomingCallAlert() {
        incomingCallRinger.stop()
        (getSystemService(NOTIFICATION_SERVICE) as NotificationManager).cancel(INCOMING_CALL_NOTIFICATION_ID)
    }

    private fun createNotificationChannels() {
        if (Build.VERSION.SDK_INT < 26) return
        val callSound = RingtoneManager.getDefaultUri(RingtoneManager.TYPE_RINGTONE)
        val callAttributes = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_NOTIFICATION_RINGTONE)
            .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
            .build()
        val callChannel = NotificationChannel(
            CALL_CHANNEL_ID,
            "Incoming calls",
            NotificationManager.IMPORTANCE_HIGH,
        ).apply {
            description = "Alerts for calls received through Viti Operator"
            setShowBadge(true)
            setSound(callSound, callAttributes)
            enableVibration(true)
            lockscreenVisibility = android.app.Notification.VISIBILITY_PUBLIC
        }
        val messageSound = RingtoneManager.getDefaultUri(RingtoneManager.TYPE_NOTIFICATION)
        val messageAttributes = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_NOTIFICATION)
            .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
            .build()
        val messageChannel = NotificationChannel(
            MESSAGE_CHANNEL_ID,
            "Received messages",
            NotificationManager.IMPORTANCE_HIGH,
        ).apply {
            description = "Alerts for SMS received through Viti Operator"
            setShowBadge(true)
            setSound(messageSound, messageAttributes)
            enableVibration(true)
            lockscreenVisibility = android.app.Notification.VISIBILITY_PRIVATE
        }
        (getSystemService(NOTIFICATION_SERVICE) as NotificationManager).apply {
            createNotificationChannels(listOf(callChannel, messageChannel))
            deleteNotificationChannel(LEGACY_CALL_CHANNEL_ID)
        }
    }

    private fun showIncomingCallNotification(peer: String) {
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) return
        val openApp = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val caller = peer.ifBlank { "Private number" }
        val answerCall = PendingIntent.getActivity(
            this,
            ANSWER_CALL_REQUEST_CODE,
            Intent(this, MainActivity::class.java)
                .setAction(ACTION_ANSWER_CALL)
                .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val rejectCall = PendingIntent.getActivity(
            this,
            REJECT_CALL_REQUEST_CODE,
            Intent(this, MainActivity::class.java)
                .setAction(ACTION_REJECT_CALL)
                .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val callerPerson = Person.Builder()
            .setName(caller)
            .setImportant(true)
            .build()
        val notification = NotificationCompat.Builder(this, CALL_CHANNEL_ID)
            .setSmallIcon(com.viti.operator.R.drawable.ic_launcher_foreground)
            .setContentTitle("Incoming call")
            .setContentText(caller)
            .setStyle(NotificationCompat.CallStyle.forIncomingCall(callerPerson, rejectCall, answerCall))
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setPriority(NotificationCompat.PRIORITY_MAX)
            .setVisibility(NotificationCompat.VISIBILITY_PUBLIC)
            .setOngoing(true)
            // IncomingCallRinger owns playback so the ringtone keeps its cadence
            // until the call is answered or rejected. Keeping the notification
            // itself silent avoids starting the same ringtone twice.
            .setSilent(true)
            .setContentIntent(openApp)
            .setFullScreenIntent(openApp, true)
            .build()
        (getSystemService(NOTIFICATION_SERVICE) as NotificationManager)
            .notify(INCOMING_CALL_NOTIFICATION_ID, notification)
    }

    private fun showIncomingMessageNotification(message: Message) {
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) return
        val notificationId = SMS_NOTIFICATION_BASE_ID + (message.id.hashCode() and 0x7fff)
        val openApp = PendingIntent.getActivity(
            this,
            notificationId,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val sender = message.from.ifBlank { "Unknown number" }
        val notification = NotificationCompat.Builder(this, MESSAGE_CHANNEL_ID)
            .setSmallIcon(com.viti.operator.R.drawable.ic_launcher_foreground)
            .setSound(RingtoneManager.getDefaultUri(RingtoneManager.TYPE_NOTIFICATION))
            .setContentTitle(sender)
            .setContentText(message.text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(message.text))
            .setCategory(NotificationCompat.CATEGORY_MESSAGE)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setVisibility(NotificationCompat.VISIBILITY_PRIVATE)
            .setAutoCancel(true)
            .setContentIntent(openApp)
            .build()
        (getSystemService(NOTIFICATION_SERVICE) as NotificationManager).notify(notificationId, notification)
    }

    private fun withMicrophonePermission(action: () -> Unit) {
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) {
            action()
        } else {
            pendingMicrophoneAction = action
            microphonePermission.launch(Manifest.permission.RECORD_AUDIO)
        }
    }

    private fun prepareShizuku() {
		if (!ensureShizuku()) return
        lifecycleScope.launch {
            runCatching { ShizukuSetup.grantSimAuthenticationAppOp(this@MainActivity) }
                .onSuccess(viewModel::showNotice)
                .onFailure { viewModel.showNotice(it.message ?: "Shizuku could not prepare the permission") }
        }
    }

	private fun enableServerLine() {
		if (!ensureShizuku()) return
		if (viewModel.phoneRouting.value.remoteSimModeEnabled) {
			viewModel.setServerIMSRegistration(true)
			return
		}
		lifecycleScope.launch {
			viewModel.showNotice("Protecting the phone before registering the server…")
			runCatching { ShizukuSetup.enableRemoteSimMode(this@MainActivity) }
				.onSuccess { payload ->
					val previousMask = JSONObject(payload).getString("previous_network_mask")
					viewModel.savePhoneRouting(PhoneRoutingSettings(true, previousMask))
					startAuthAgent()
					viewModel.claimSIMAuthentication { claimResult ->
						if (claimResult.isFailure) {
							lifecycleScope.launch {
								runCatching { ShizukuSetup.disableRemoteSimMode(this@MainActivity, previousMask) }
								viewModel.savePhoneRouting(PhoneRoutingSettings())
								stopAuthAgent()
								viewModel.showNotice("The server did not accept this SIM; native phone service was restored")
							}
							return@claimSIMAuthentication
						}
						viewModel.setServerIMSRegistration(true) { serverResult ->
							if (serverResult.isFailure) {
								lifecycleScope.launch {
									runCatching { ShizukuSetup.disableRemoteSimMode(this@MainActivity, previousMask) }
									viewModel.savePhoneRouting(PhoneRoutingSettings())
									stopAuthAgent()
									viewModel.showNotice("The server could not register; native phone service was restored")
								}
							}
						}
					}
				}
				.onFailure { viewModel.showNotice(it.message ?: "Could not isolate native phone service") }
		}
	}

	private fun returnLineToPhone() {
		if (!ensureShizuku()) return
		val routing = viewModel.phoneRouting.value
		if (!routing.remoteSimModeEnabled || routing.restoreNetworkMask.isBlank()) {
			viewModel.showNotice("The network configuration to restore on the phone is unknown")
			return
		}
		viewModel.setServerIMSRegistration(false) { serverResult ->
			if (serverResult.isFailure) return@setServerIMSRegistration
			lifecycleScope.launch {
				runCatching { ShizukuSetup.disableRemoteSimMode(this@MainActivity, routing.restoreNetworkMask) }
					.onSuccess {
						viewModel.savePhoneRouting(PhoneRoutingSettings())
						viewModel.showNotice("The line is using native phone service again")
					}
					.onFailure { error ->
						// Avoid leaving the line intentionally unreachable if Android could not be restored.
						viewModel.setServerIMSRegistration(true)
						viewModel.showNotice("The phone could not be restored; the server was registered again: ${error.message.orEmpty()}")
					}
			}
		}
	}

	private fun ensureShizuku(): Boolean {
		if (!ShizukuSetup.available()) {
			viewModel.showNotice("Start Shizuku and try again")
			return false
		}
		if (!ShizukuSetup.authorized()) {
			ShizukuSetup.requestAuthorization()
			viewModel.showNotice("Authorize Viti Operator in the Shizuku dialog")
			return false
		}
		return true
	}

    private fun startAuthAgent() {
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
        ContextCompat.startForegroundService(
            this,
            Intent(this, RemoteAuthService::class.java).setAction(RemoteAuthService.ACTION_CLAIM),
        )
    }

    private fun stopAuthAgent() {
        startService(Intent(this, RemoteAuthService::class.java).setAction(RemoteAuthService.ACTION_STOP))
    }

    private fun requestContacts() {
        if (ContextCompat.checkSelfPermission(this, Manifest.permission.READ_CONTACTS) == PackageManager.PERMISSION_GRANTED) {
            loadContacts()
        } else {
            contactsPermission.launch(Manifest.permission.READ_CONTACTS)
        }
    }

    private fun loadContacts() {
        lifecycleScope.launch {
            val contacts = withContext(Dispatchers.IO) { ContactsRepository(applicationContext).load() }
            viewModel.setContacts(contacts)
        }
    }

    companion object {
        private const val CALL_CHANNEL_ID = "incoming_calls_v2"
        private const val MESSAGE_CHANNEL_ID = "incoming_messages_v1"
        private const val LEGACY_CALL_CHANNEL_ID = "incoming_calls"
        private const val INCOMING_CALL_NOTIFICATION_ID = 4402
        private const val SMS_NOTIFICATION_BASE_ID = 5000
        private const val ANSWER_CALL_REQUEST_CODE = 4403
        private const val REJECT_CALL_REQUEST_CODE = 4404
        private const val DEBUG_SERVER_URL = "com.viti.operator.test.SERVER_URL"
        private const val DEBUG_API_KEY = "com.viti.operator.test.API_KEY"
        private const val DEBUG_INCOMING_CALL = "com.viti.operator.test.INCOMING_CALL"
        private const val ACTION_ANSWER_CALL = "com.viti.operator.action.ANSWER_CALL"
        private const val ACTION_REJECT_CALL = "com.viti.operator.action.REJECT_CALL"
    }
}
