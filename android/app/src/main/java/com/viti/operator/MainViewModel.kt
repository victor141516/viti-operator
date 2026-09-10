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
import com.viti.operator.data.ServerContactsSnapshot
import com.viti.operator.data.SessionCountryDetector
import com.viti.operator.data.SettingsStore
import com.viti.operator.data.IMSRegistrationState
import com.viti.operator.data.PhoneRoutingSettings
import com.viti.operator.data.RecordingSettings
import com.viti.operator.data.VitiApi
import com.viti.operator.data.VoiceUiState
import kotlinx.coroutines.Job
import kotlinx.coroutines.async
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
import java.util.UUID

data class AppUiState(
    val status: RuntimeStatus = RuntimeStatus(),
    val messages: List<Message> = emptyList(),
    val calls: List<Call> = emptyList(),
    val contacts: List<PhoneContact> = emptyList(),
    val contactsAvailable: Boolean = false,
    val voice: VoiceUiState = VoiceUiState(),
	val serverIMS: IMSRegistrationState = IMSRegistrationState(),
    val recording: RecordingSettings = RecordingSettings(),
    val topology: ConnectionTopology = ConnectionTopology(),
    val clientId: String = "",
    val refreshing: Boolean = true,
    val busy: Boolean = false,
    val notice: String = "",
)

internal fun shouldReloadContacts(
    cachedSettings: ServerSettings?,
    cachedVersion: Long?,
    currentSettings: ServerSettings,
    serverVersion: Long,
): Boolean = cachedSettings != currentSettings || cachedVersion != serverVersion

class MainViewModel(
    private val settingsStore: SettingsStore,
    private val countryDetector: SessionCountryDetector,
) : ViewModel() {
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
    private var contactsCacheSettings: ServerSettings? = null
    private var contactsCacheVersion: Long? = null
    private val incomingMessageTracker = IncomingMessageTracker()
    private val sessionGeneration = UUID.randomUUID().toString()
    private val sessionCountry = viewModelScope.async { countryDetector.detect(sessionGeneration) }

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
        val refreshSettings = settings.value
        val api = VitiApi(refreshSettings)
        runCatching {
            clientIdentity?.let { api.heartbeat(it) }
            val status = api.status()
            val contacts = if (shouldReloadContacts(contactsCacheSettings, contactsCacheVersion, refreshSettings, status.contactsVersion)) api.contacts() else null
            RefreshData(status, api.messages(), api.calls(), contacts, api.imsRegistration(), api.recordingSettings(), api.topology())
        }.onSuccess { data ->
            val currentMessageSource = settings.value.serverUrl.trim().trimEnd('/')
            val newIncomingMessages = incomingMessageTracker.update(currentMessageSource, data.messages)
            data.contacts?.let {
                contactsCacheSettings = refreshSettings
                contactsCacheVersion = it.version
            }
            _uiState.value = _uiState.value.copy(
                status = data.status,
                messages = data.messages,
                calls = data.calls,
                contacts = data.contacts?.contacts ?: _uiState.value.contacts,
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
        runCatching { VitiApi(settings.value).startCall(normalizeCallDestination(to)) }
            .onSuccess {
                _uiState.value = _uiState.value.copy(busy = false, notice = "Call started")
                refresh(silent = true)
            }
            .onFailure { error ->
                _uiState.value = _uiState.value.copy(busy = false, notice = error.message ?: "Could not place the call")
            }
    }

    suspend fun normalizeCallDestination(to: String): String {
        val number = to.trim()
        val country = if (number.startsWith("+")) "" else sessionCountry.await()
            .takeIf { it.sessionGeneration == sessionGeneration }
            ?.country
            .orEmpty()
        return VitiApi(settings.value).normalizeCallDestination(number, country)
    }

    fun setVoiceState(value: VoiceUiState) {
        _uiState.value = _uiState.value.copy(voice = value)
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
                        contactsCacheSettings = attemptedSettings
                        contactsCacheVersion = contacts.version
                        _uiState.value = _uiState.value.copy(contacts = contacts.contacts, notice = "Contacts synchronized")
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

    class Factory(
        private val store: SettingsStore,
        private val countryDetector: SessionCountryDetector,
    ) : ViewModelProvider.Factory {
        @Suppress("UNCHECKED_CAST")
        override fun <T : ViewModel> create(modelClass: Class<T>): T = MainViewModel(store, countryDetector) as T
    }

    private data class RefreshData(
        val status: RuntimeStatus,
        val messages: List<Message>,
        val calls: List<Call>,
        val contacts: ServerContactsSnapshot?,
		val serverIMS: IMSRegistrationState,
        val recording: RecordingSettings,
        val topology: ConnectionTopology,
    )
}
