package com.viti.operator

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.media.AudioAttributes
import android.media.RingtoneManager
import android.content.Intent
import android.content.ComponentName
import android.content.Context
import android.content.ServiceConnection
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.database.ContentObserver
import android.provider.ContactsContract
import android.provider.Settings
import android.net.Uri
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.core.content.ContextCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
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
import com.viti.operator.voice.VoiceCallService
import com.viti.operator.voice.IncomingCallPresentationStatus
import com.viti.operator.voice.fullScreenIntentAllowed
import kotlinx.coroutines.launch
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withContext
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import org.json.JSONObject

class MainActivity : ComponentActivity() {
    private val settingsStore by lazy { SettingsStore(applicationContext) }
    private val viewModel by viewModels<MainViewModel> { MainViewModel.Factory(settingsStore) }
    private var voiceService by mutableStateOf<VoiceCallService?>(null)
    private var voiceServiceBound = false
    private var voiceSnapshotJob: Job? = null
    private val clientIdentity by lazy { vitiClientIdentity() }
    private var pendingMicrophoneAction: (() -> Unit)? = null
    private var contactsObserver: ContentObserver? = null
    private var contactReloadJob: Job? = null
    private var callPresentationStatus by mutableStateOf(IncomingCallPresentationStatus())
    private val voiceConnection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName, binder: android.os.IBinder) {
            val service = (binder as VoiceCallService.LocalBinder).service
            voiceService = service
            voiceServiceBound = true
            refreshCallPresentationStatus()
            service.detachVoiceView()
            voiceSnapshotJob?.cancel()
            voiceSnapshotJob = lifecycleScope.launch {
                service.snapshot.collect { viewModel.setVoiceState(it.voice) }
            }
            handleNotificationIntent(intent)
        }

        override fun onServiceDisconnected(name: ComponentName) {
            voiceSnapshotJob?.cancel()
            voiceSnapshotJob = null
            voiceService = null
            voiceServiceBound = false
        }
    }
    private val notificationPermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) {
        refreshCallPresentationStatus()
    }
    private val bluetoothPermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) { }
    private val microphonePermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        val action = pendingMicrophoneAction
        pendingMicrophoneAction = null
        if (granted) {
            requestBluetoothControlIfNeeded()
            action?.invoke()
        } else {
            viewModel.showNotice("Microphone access is required for calls")
        }
    }
    private val contactsPermission = registerForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        if (granted) {
            enableContactSync()
        } else viewModel.showNotice("Contacts permission was not granted")
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        applyDebugServerOverride(intent)
        publishClientIdentity()
        createNotificationChannels()
        refreshCallPresentationStatus()
        lifecycleScope.launch {
            viewModel.incomingMessages.collect(::showIncomingMessageNotification)
        }
        ContextCompat.startForegroundService(this, Intent(this, VoiceCallService::class.java))
        setContent {
            VitiTheme {
                voiceService?.let { service ->
                    VitiApp(
                        viewModel = viewModel,
                        voiceRuntime = service.runtime,
                        voiceClient = service.voiceClient,
                        onPlaceCall = ::placeCall,
                        onAnswerCall = { answerCall() },
                        onRejectCall = { service.runtime.reject() },
                        onHangupCall = { service.runtime.hangup() },
                        onSetMuted = service.runtime::setMuted,
                        onSendDtmf = service.runtime::sendDtmf,
                        onSelectAudioEndpoint = service.runtime::requestEndpoint,
                        onPrepareShizuku = ::prepareShizuku,
                        onStartAuthAgent = ::startAuthAgent,
                        onStopAuthAgent = ::stopAuthAgent,
						onEnableServerLine = ::enableServerLine,
						onReturnLineToPhone = ::returnLineToPhone,
                        onRequestContacts = ::requestContacts,
                        callPresentationStatus = callPresentationStatus,
                        onRequestCallNotifications = ::requestCallNotificationAccess,
                        onOpenFullScreenCallSettings = ::openFullScreenCallSettings,
                        onOpenCallChannelSettings = ::openCallChannelSettings,
                    )
                }
            }
        }
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        applyDebugServerOverride(intent)
        handleNotificationIntent(intent)
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
        bindService(Intent(this, VoiceCallService::class.java), voiceConnection, Context.BIND_AUTO_CREATE)
        publishClientIdentity()
        // Existing installs may have granted READ_CONTACTS for the old local-only
        // picker. Upload starts only after the separate Viti sync disclosure was
        // accepted and persisted.
        lifecycleScope.launch {
            val permissionGranted = ContextCompat.checkSelfPermission(this@MainActivity, Manifest.permission.READ_CONTACTS) == PackageManager.PERMISSION_GRANTED
            if (permissionGranted && settingsStore.contactSyncConsent.first()) {
                observeContacts()
                loadContacts()
            }
        }
    }

    override fun onResume() {
        super.onResume()
        refreshCallPresentationStatus()
    }

    override fun onStop() {
        contactsObserver?.let(contentResolver::unregisterContentObserver)
        contactsObserver = null
        contactReloadJob?.cancel()
        voiceSnapshotJob?.cancel()
        voiceSnapshotJob = null
        voiceService?.detachVoiceView()
        if (voiceServiceBound) {
            unbindService(voiceConnection)
            voiceServiceBound = false
        }
        voiceService = null
        super.onStop()
    }

    private fun publishClientIdentity() {
        viewModel.setClientIdentity(clientIdentity)
    }

    private fun placeCall(number: String) = withMicrophonePermission { voiceService?.runtime?.placeCall(number) }

    private fun answerCall(expectedToken: String? = null) = withMicrophonePermission { voiceService?.runtime?.answer(expectedToken) }

    private fun handleNotificationIntent(intent: Intent) {
        if (voiceService == null) return
        when (intent.action) {
            ACTION_ANSWER_CALL -> {
                intent.action = null
                answerCall(intent.getStringExtra(VoiceCallService.EXTRA_LEG_TOKEN))
            }
        }
    }

    private fun createNotificationChannels() {
        if (Build.VERSION.SDK_INT < 26) return
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
            createNotificationChannel(messageChannel)
            deleteNotificationChannel(LEGACY_CALL_CHANNEL_ID)
        }
    }

    private fun refreshCallPresentationStatus() {
        val manager = getSystemService(NotificationManager::class.java)
        val channelImportance = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            manager.getNotificationChannel(VoiceCallService.CALL_CHANNEL_ID)?.importance
                ?: NotificationManager.IMPORTANCE_UNSPECIFIED
        } else {
            null
        }
        val platformAllowsFullScreen = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            manager.canUseFullScreenIntent()
        } else {
            true
        }
        callPresentationStatus = IncomingCallPresentationStatus(
            notificationsEnabled = NotificationManagerCompat.from(this).areNotificationsEnabled(),
            callChannelImportance = channelImportance,
            fullScreenIntentAllowed = fullScreenIntentAllowed(Build.VERSION.SDK_INT, platformAllowsFullScreen),
            fullScreenSettingAvailable = Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE,
        )
    }

    private fun requestCallNotificationAccess() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
            return
        }
        openSettings(appNotificationSettingsIntent())
    }

    private fun openFullScreenCallSettings() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.UPSIDE_DOWN_CAKE) return
        openSettings(
            Intent(Settings.ACTION_MANAGE_APP_USE_FULL_SCREEN_INTENT)
                .setData(Uri.parse("package:$packageName")),
        )
    }

    private fun openCallChannelSettings() {
        val intent = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            Intent(Settings.ACTION_CHANNEL_NOTIFICATION_SETTINGS)
                .putExtra(Settings.EXTRA_APP_PACKAGE, packageName)
                .putExtra(Settings.EXTRA_CHANNEL_ID, VoiceCallService.CALL_CHANNEL_ID)
        } else {
            appNotificationSettingsIntent()
        }
        openSettings(intent)
    }

    private fun appNotificationSettingsIntent(): Intent =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                .putExtra(Settings.EXTRA_APP_PACKAGE, packageName)
        } else {
            Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
                .setData(Uri.parse("package:$packageName"))
        }

    private fun openSettings(intent: Intent) {
        runCatching { startActivity(intent) }
            .onFailure { viewModel.showNotice("Android did not provide this settings screen") }
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
        val senderNumber = message.from.ifBlank { "Unknown number" }
        val sender = message.contactName.ifBlank { senderNumber }
        val notification = NotificationCompat.Builder(this, MESSAGE_CHANNEL_ID)
            .setSmallIcon(com.viti.operator.R.drawable.ic_launcher_foreground)
            .setSound(RingtoneManager.getDefaultUri(RingtoneManager.TYPE_NOTIFICATION))
            .setContentTitle(sender)
            .setContentText(if (message.contactName.isBlank()) message.text else "$senderNumber · ${message.text}")
            .setStyle(NotificationCompat.BigTextStyle().setSummaryText(if (message.contactName.isBlank()) null else senderNumber).bigText(message.text))
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
            requestBluetoothControlIfNeeded()
            action()
        } else {
            pendingMicrophoneAction = action
            microphonePermission.launch(Manifest.permission.RECORD_AUDIO)
        }
    }

    private fun requestBluetoothControlIfNeeded() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.BLUETOOTH_CONNECT) != PackageManager.PERMISSION_GRANTED
        ) {
            bluetoothPermission.launch(Manifest.permission.BLUETOOTH_CONNECT)
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
            enableContactSync()
        } else {
            contactsPermission.launch(Manifest.permission.READ_CONTACTS)
        }
    }

    private fun enableContactSync() {
        lifecycleScope.launch {
            settingsStore.setContactSyncConsent(true)
            observeContacts()
            loadContacts()
        }
    }

    private fun loadContacts() {
        contactReloadJob?.cancel()
        contactReloadJob = lifecycleScope.launch {
            runCatching { withContext(Dispatchers.IO) { ContactsRepository(applicationContext).load() } }
                .onSuccess(viewModel::synchronizeContacts)
                .onFailure { viewModel.showNotice(it.message ?: "Could not read contacts") }
        }
    }

    private fun observeContacts() {
        if (contactsObserver != null) return
        contactsObserver = object : ContentObserver(Handler(Looper.getMainLooper())) {
            override fun onChange(selfChange: Boolean) {
                contactReloadJob?.cancel()
                contactReloadJob = lifecycleScope.launch {
                    delay(750)
                    runCatching { withContext(Dispatchers.IO) { ContactsRepository(applicationContext).load() } }
                        .onSuccess(viewModel::synchronizeContacts)
                        .onFailure { viewModel.showNotice(it.message ?: "Could not read contacts") }
                }
            }
        }.also { contentResolver.registerContentObserver(ContactsContract.AUTHORITY_URI, true, it) }
    }

    companion object {
        private const val MESSAGE_CHANNEL_ID = "incoming_messages_v1"
        private const val LEGACY_CALL_CHANNEL_ID = "incoming_calls"
        private const val SMS_NOTIFICATION_BASE_ID = 5000
        private const val DEBUG_SERVER_URL = "com.viti.operator.test.SERVER_URL"
        private const val DEBUG_API_KEY = "com.viti.operator.test.API_KEY"
        const val ACTION_ANSWER_CALL = "com.viti.operator.action.ANSWER_CALL"
    }
}
