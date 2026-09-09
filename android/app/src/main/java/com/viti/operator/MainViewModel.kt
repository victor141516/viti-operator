package com.viti.operator

import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewModelScope
import com.viti.operator.auth.RemoteAuthService
import com.viti.operator.data.Call
import com.viti.operator.data.ClientIdentity
import com.viti.operator.data.ConnectionTopology
import com.viti.operator.data.DeviceContactsSnapshot
import com.viti.operator.data.Message
import com.viti.operator.data.PhoneContact
import com.viti.operator.data.asPhoneContacts
import com.viti.operator.data.RuntimeStatus
import com.viti.operator.data.ServerSettings
import com.viti.operator.data.SettingsStore
import com.viti.operator.data.IMSRegistrationState
import com.viti.operator.data.PhoneRoutingSettings
import com.viti.operator.data.RecordingSettings
import com.viti.operator.data.VitiApi
import com.viti.operator.data.VoiceProvisioning
import com.viti.operator.data.VoiceUiState
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import org.json.JSONObject

data class AppUiState(
    val status: RuntimeStatus = RuntimeStatus(),
    val messages: List<Message> = emptyList(),
    val calls: List<Call> = emptyList(),
    val contacts: List<PhoneContact> = emptyList(),
    val contactsAvailable: Boolean = false,
    val voiceProvisioning: VoiceProvisioning = VoiceProvisioning(),
    val voice: VoiceUiState = VoiceUiState(),
	val serverIMS: IMSRegistrationState = IMSRegistrationState(),
    val recording: RecordingSettings = RecordingSettings(),
    val topology: ConnectionTopology = ConnectionTopology(),
    val clientId: String = "",
    val refreshing: Boolean = true,
    val busy: Boolean = false,
    val notice: String = "",
)

class MainViewModel(private val settingsStore: SettingsStore) : ViewModel() {
    val settings: StateFlow<ServerSettings> = settingsStore.settings.stateIn(
        viewModelScope,
        SharingStarted.WhileSubscribed(5_000),
        ServerSettings(),
    )
    val authService = RemoteAuthService.state
	val phoneRouting: StateFlow<PhoneRoutingSettings> = settingsStore.phoneRouting.stateIn(
		viewModelScope,
		SharingStarted.WhileSubscribed(5_000),
		PhoneRoutingSettings(),
	)

    private val _uiState = MutableStateFlow(AppUiState())
    val uiState = _uiState.asStateFlow()
    private val _incomingMessages = MutableSharedFlow<Message>(extraBufferCapacity = 16)
    val incomingMessages = _incomingMessages.asSharedFlow()
    private var clientIdentity: ClientIdentity? = null
    private var unversionedContactSnapshot: DeviceContactsSnapshot? = null
    private var latestContactSnapshot: DeviceContactsSnapshot? = null
    private var pendingContactSnapshot: DeviceContactsSnapshot? = null
    private var contactRevisionJob: Job? = null
    private var contactSyncJob: Job? = null
    private var contactSyncSettings: ServerSettings? = null
    private val incomingMessageTracker = IncomingMessageTracker()

    init {
        viewModelScope.launch {
            settings.collectLatest { currentSettings ->
                if (contactSyncSettings != currentSettings) {
                    latestContactSnapshot?.let { pendingContactSnapshot = it }
                    startContactSync()
                }
                refresh()
            }
        }
        viewModelScope.launch {
            while (true) {
                delay(5_000)
                refresh(silent = true)
            }
        }
    }

    fun refresh(silent: Boolean = false) = viewModelScope.launch {
        if (!silent) _uiState.value = _uiState.value.copy(refreshing = true)
        val api = VitiApi(settings.value)
        runCatching {
            clientIdentity?.let { api.heartbeat(it) }
            RefreshData(api.status(), api.messages(), api.calls(), api.contacts(), api.voice(), api.imsRegistration(), api.recordingSettings(), api.topology())
        }.onSuccess { data ->
            val currentMessageSource = settings.value.serverUrl.trim().trimEnd('/')
            val newIncomingMessages = incomingMessageTracker.update(currentMessageSource, data.messages)
            _uiState.value = _uiState.value.copy(
                status = data.status,
                messages = data.messages,
                calls = data.calls,
                contacts = data.contacts,
                voiceProvisioning = data.voice,
				serverIMS = data.serverIMS,
                recording = data.recording,
                topology = data.topology,
                refreshing = false,
                notice = "",
            )
            newIncomingMessages.forEach(_incomingMessages::tryEmit)
        }.onFailure { error ->
            _uiState.value = _uiState.value.copy(
                refreshing = false,
                notice = error.message ?: "Could not contact the server",
            )
        }
    }

    fun setClientIdentity(identity: ClientIdentity) {
        clientIdentity = identity
        _uiState.value = _uiState.value.copy(clientId = identity.id)
        startContactSync()
        refresh(silent = true)
    }

    fun claimSIMAuthentication(onResult: (Result<Unit>) -> Unit) = viewModelScope.launch {
        val identity = clientIdentity
        if (identity == null) {
            val result = Result.failure<Unit>(IllegalStateException("The phone has not published its identity yet"))
            onResult(result)
            return@launch
        }
        val result = runCatching {
            val api = VitiApi(settings.value)
            api.heartbeat(identity)
            api.setSIMAuthAssignment(identity, true)
        }
        result.onSuccess { refresh(silent = true) }
            .onFailure { error -> showNotice(error.message ?: "The server did not accept this SIM") }
        onResult(result)
    }

    fun sendMessage(to: String, text: String) = viewModelScope.launch {
        _uiState.value = _uiState.value.copy(busy = true, notice = "Sending message…")
        runCatching { VitiApi(settings.value).sendMessage(to.trim(), text.trim()) }
            .onSuccess {
                _uiState.value = _uiState.value.copy(busy = false, notice = "Send started")
                refresh(silent = true)
            }
            .onFailure { error ->
                _uiState.value = _uiState.value.copy(busy = false, notice = error.message ?: "Could not send the message")
            }
    }

    fun startCall(to: String) = viewModelScope.launch {
        _uiState.value = _uiState.value.copy(busy = true, notice = "Starting call…")
        runCatching { VitiApi(settings.value).startCall(to.trim()) }
            .onSuccess {
                _uiState.value = _uiState.value.copy(busy = false, notice = "Call started")
                refresh(silent = true)
            }
            .onFailure { error ->
                _uiState.value = _uiState.value.copy(busy = false, notice = error.message ?: "Could not place the call")
            }
    }

    fun onVoiceEvent(type: String, peer: String = "", cause: String = "", traceId: String = "", participantId: String = "") {
        val previous = _uiState.value.voice
        val nextPeer = peer.ifBlank { previous.peer }
        val detail = when (type) {
            "connected" -> "Connecting the line…"
            "registered" -> "Line ready for calls"
            "incoming" -> "Incoming call from $nextPeer"
            "calling" -> "Calling $nextPeer…"
            "ringing" -> "Ringing…"
            "active" -> "Call in progress"
            "media" -> when {
                "sent=true" in cause && "received=true" in cause -> "Call in progress · two-way audio"
                "sent=true" in cause -> "Call in progress · sending audio"
                else -> "Call in progress"
            }
            "mute-changed" -> previous.detail
            "dtmf" -> previous.detail
            "dtmf-failed" -> if (cause.isBlank()) "Could not send the DTMF tone" else cause
            "ended" -> "Call ended"
            "failed" -> if (cause.isBlank()) "The call failed" else "The call failed: $cause"
            "registration-failed" -> if (cause.isBlank()) "Could not register the line" else "Registration failed: $cause"
            else -> "Telephony disconnected"
        }
        // A failed SIP session does not unregister the UA. Return to the ready state so
        // the user can retry without restarting the app; registration failures remain fatal.
        val stableState = when (type) {
            "ended", "failed" -> "registered"
            "dtmf", "dtmf-failed", "mute-changed" -> previous.state
            else -> type
        }
        _uiState.value = _uiState.value.copy(
            voice = VoiceUiState(
                state = stableState,
                peer = if (type in setOf("ended", "failed")) "" else nextPeer,
                detail = detail,
                microphoneMuted = when {
                    type == "mute-changed" -> cause == "muted=true"
                    type in setOf("incoming", "calling", "ended", "failed") -> false
                    else -> previous.microphoneMuted
                },
            ),
        )
        if (type == "active" && traceId.isNotBlank() && participantId.isNotBlank()) {
            clientIdentity?.let { identity ->
                viewModelScope.launch {
                    runCatching { VitiApi(settings.value).claimRecordingParticipant(traceId, participantId, identity) }
                }
            }
        }
        recordVoiceDiagnostic(
            JSONObject()
                .put("at_ms", System.currentTimeMillis())
                .put("source", "android-view-model")
                .put("kind", "voice-state.transition")
                .put("trace_id", traceId)
                .put(
                    "data",
                    JSONObject()
                        .put("event", type)
                        .put("peer", peer)
                        .put("cause", cause)
                        .put("previous", previous.toString())
                        .put("current", _uiState.value.voice.toString()),
                )
                .toString(),
        )
    }

    fun recordVoiceDiagnostic(payload: String) = viewModelScope.launch {
        runCatching { VitiApi(settings.value).recordDiagnostic(JSONObject(payload)) }
    }

    fun saveSettings(value: ServerSettings) = viewModelScope.launch {
        settingsStore.save(value)
        _uiState.value = _uiState.value.copy(notice = "Connection saved")
    }

	fun setServerIMSRegistration(enabled: Boolean, onResult: (Result<Unit>) -> Unit = {}) = viewModelScope.launch {
		_uiState.value = _uiState.value.copy(busy = true, notice = if (enabled) "Registering the server with IMS…" else "Unregistering the server from IMS…")
		val result = runCatching { VitiApi(settings.value).setIMSRegistration(enabled) }
		result.onSuccess { state ->
			_uiState.value = _uiState.value.copy(
				busy = false,
				serverIMS = state,
				notice = if (enabled) "The server is starting IMS registration" else "The server is no longer registered with IMS",
			)
			refresh(silent = true)
		}.onFailure { error ->
			_uiState.value = _uiState.value.copy(busy = false, notice = error.message ?: "Could not change server IMS registration")
		}
		onResult(result.map { })
	}

    fun setRecordingEnabled(enabled: Boolean) = viewModelScope.launch {
        _uiState.value = _uiState.value.copy(busy = true, notice = if (enabled) "Enabling call recording…" else "Disabling call recording…")
        runCatching { VitiApi(settings.value).setRecordingEnabled(enabled) }
            .onSuccess { recording ->
                _uiState.value = _uiState.value.copy(
                    busy = false,
                    recording = recording,
                    notice = if (recording.enabled) "New calls will be recorded" else "New calls will not be recorded",
                )
            }
            .onFailure { error ->
                _uiState.value = _uiState.value.copy(busy = false, notice = error.message ?: "Could not change call recording")
            }
    }

	fun savePhoneRouting(value: PhoneRoutingSettings) = viewModelScope.launch {
		settingsStore.savePhoneRouting(value)
	}

    fun showNotice(message: String) {
        _uiState.value = _uiState.value.copy(notice = message)
    }

    fun synchronizeContacts(snapshot: DeviceContactsSnapshot) {
        val localContacts = snapshot.asPhoneContacts()
        unversionedContactSnapshot = snapshot
        _uiState.value = _uiState.value.copy(
            contacts = localContacts,
            contactsAvailable = true,
            notice = if (localContacts.isEmpty()) "No contacts have phone numbers" else "Synchronizing contacts…",
        )
        startContactRevisioning()
    }

    private fun startContactRevisioning() {
        if (unversionedContactSnapshot == null || contactRevisionJob?.isActive == true) return
        contactRevisionJob = viewModelScope.launch {
            while (true) {
                val snapshot = unversionedContactSnapshot ?: break
                unversionedContactSnapshot = null
                val versionedSnapshot = snapshot.copy(revision = settingsStore.nextContactSnapshotRevision())
                latestContactSnapshot = versionedSnapshot
                pendingContactSnapshot = versionedSnapshot
                startContactSync()
            }
        }
    }

    private fun startContactSync() {
        if (pendingContactSnapshot == null || clientIdentity == null || contactSyncJob?.isActive == true) return
        contactSyncJob = viewModelScope.launch {
            var retryDelay = 1_000L
            while (true) {
                val snapshot = pendingContactSnapshot ?: break
                val identity = clientIdentity ?: break
                val attemptedSettings = settings.value
                val result = runCatching {
                    val api = VitiApi(attemptedSettings)
                    api.replaceContacts(identity, snapshot)
                    api.contacts()
                }
                result.onSuccess { contacts ->
                    if (settings.value == attemptedSettings) {
                        if (pendingContactSnapshot == snapshot) pendingContactSnapshot = null
                        contactSyncSettings = attemptedSettings
                        _uiState.value = _uiState.value.copy(contacts = contacts, notice = "Contacts synchronized")
                    }
                    retryDelay = 1_000L
                }.onFailure { error ->
                    _uiState.value = _uiState.value.copy(notice = error.message ?: "Could not synchronize contacts; retrying")
                    delay(retryDelay)
                    retryDelay = (retryDelay * 2).coerceAtMost(30_000L)
                }
            }
        }
    }

    fun contactNameFor(number: String): String = _uiState.value.contacts.firstOrNull {
        it.number.filter(Char::isDigit) == number.filter(Char::isDigit)
    }?.name.orEmpty()

    class Factory(private val store: SettingsStore) : ViewModelProvider.Factory {
        @Suppress("UNCHECKED_CAST")
        override fun <T : ViewModel> create(modelClass: Class<T>): T = MainViewModel(store) as T
    }

    private data class RefreshData(
        val status: RuntimeStatus,
        val messages: List<Message>,
        val calls: List<Call>,
        val contacts: List<PhoneContact>,
        val voice: VoiceProvisioning,
		val serverIMS: IMSRegistrationState,
        val recording: RecordingSettings,
        val topology: ConnectionTopology,
    )
}
