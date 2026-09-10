package com.viti.operator.car

import android.Manifest
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.content.pm.PackageManager
import android.os.Build
import android.os.IBinder
import androidx.core.content.ContextCompat
import com.viti.operator.data.SettingsStore
import com.viti.operator.data.VitiApi
import com.viti.operator.voice.VoiceCallService
import com.viti.operator.voice.VoiceCallSnapshot
import com.viti.operator.voice.TELECOM_FEATURE
import com.viti.operator.voice.supportsCoreTelecom
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.cancel
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch

internal data class CarCallState(
    val coreTelecomAvailable: Boolean = false,
    val serviceConnected: Boolean = false,
    val serviceDurable: Boolean = false,
    val loadingDirectory: Boolean = true,
    val targets: List<CarDialTarget> = emptyList(),
    val call: VoiceCallSnapshot = VoiceCallSnapshot(),
    val directoryError: String = "",
    val notice: String = "",
)

internal interface CarCallController {
    val state: StateFlow<CarCallState>
    fun start()
    fun refresh()
    fun call(target: CarDialTarget)
    fun clearNotice()
    fun close()
}

/**
 * Volatile Android Auto adapter. Destroying it only unbinds from the process-owned
 * call service; it never hangs up or stops that service.
 */
internal class CarCallCoordinator(context: Context) : CarCallController {
    private val appContext = context.applicationContext
    private val settingsStore = SettingsStore(appContext)
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val _state = MutableStateFlow(CarCallState())
    override val state: StateFlow<CarCallState> = _state.asStateFlow()
    private var service: VoiceCallService? = null
    private var bound = false
    private var started = false
    private var directoryJob: Job? = null
    private var snapshotJob: Job? = null
    private var startRequestAccepted = false
    private var startFailureDetail = ""

    private val connection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName, binder: IBinder) {
            val connected = (binder as VoiceCallService.LocalBinder).service
            service = connected
            val durable = startRequestAccepted || connected.startedForBackgroundCalls
            _state.value = _state.value.copy(
                coreTelecomAvailable = connected.coreTelecomAvailable,
                serviceConnected = true,
                serviceDurable = durable,
                notice = if (durable) "" else backgroundStartFailureNotice(startFailureDetail),
            )
            snapshotJob?.cancel()
            snapshotJob = scope.launch {
                connected.snapshot.collect { snapshot ->
                    _state.value = _state.value.copy(call = snapshot)
                }
            }
        }

        override fun onServiceDisconnected(name: ComponentName) {
            snapshotJob?.cancel()
            snapshotJob = null
            service = null
            _state.value = _state.value.copy(
                serviceConnected = false,
                serviceDurable = false,
                notice = "Call service disconnected. Open Viti on your phone when safe.",
            )
        }
    }

    override fun start() {
        if (started) return
        started = true
        val capable = supportsCoreTelecom(
            Build.VERSION.SDK_INT,
            appContext.packageManager.hasSystemFeature(TELECOM_FEATURE),
        )
        _state.value = _state.value.copy(coreTelecomAvailable = capable)
        if (!capable) return

        val serviceIntent = Intent(appContext, VoiceCallService::class.java)
        val startError = runCatching {
            ContextCompat.startForegroundService(appContext, serviceIntent)
        }.exceptionOrNull()
        startRequestAccepted = startError == null
        startFailureDetail = startError?.message.orEmpty()
        val didBind = runCatching {
            appContext.bindService(serviceIntent, connection, Context.BIND_AUTO_CREATE)
        }.getOrDefault(false)
        bound = didBind
        if (!didBind) {
            _state.value = _state.value.copy(
                serviceDurable = false,
                notice = backgroundStartFailureNotice(startFailureDetail),
            )
        }
        refresh()
    }

    override fun refresh() {
        if (!started || !_state.value.coreTelecomAvailable) return
        directoryJob?.cancel()
        _state.value = _state.value.copy(loadingDirectory = true, directoryError = "")
        directoryJob = scope.launch {
            runCatching {
                val settings = settingsStore.settings.first()
                val api = VitiApi(settings)
                coroutineScope {
                    val calls = async { api.calls() }
                    val contacts = async { api.contacts().contacts }
                    buildCarDialTargets(calls.await(), contacts.await())
                }
            }.onSuccess { targets ->
                _state.value = _state.value.copy(loadingDirectory = false, targets = targets)
            }.onFailure { error ->
                _state.value = _state.value.copy(
                    loadingDirectory = false,
                    directoryError = error.message ?: "Could not load calls and contacts",
                )
            }
        }
    }

    override fun call(target: CarDialTarget) {
        val current = _state.value
        val connected = service
        val notice = carCallBlockReason(current, connected != null, hasMicrophonePermission())
        if (notice.isNotBlank()) {
            _state.value = current.copy(notice = notice)
            return
        }
        val activeService = connected ?: return
        activeService.runtime.placeCall(target.number, target.name)
    }

    override fun clearNotice() {
        _state.value = _state.value.copy(notice = "")
    }

    override fun close() {
        directoryJob?.cancel()
        snapshotJob?.cancel()
        if (bound) runCatching { appContext.unbindService(connection) }
        bound = false
        service = null
        scope.cancel()
    }

    private fun hasMicrophonePermission(): Boolean =
        ContextCompat.checkSelfPermission(appContext, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED
}

internal fun carCallBlockReason(state: CarCallState, servicePresent: Boolean, microphoneGranted: Boolean): String = when {
    !state.coreTelecomAvailable ->
        "This phone cannot expose Viti calls to Android Auto because Core-Telecom is unavailable."
    !microphoneGranted ->
        "Complete microphone permission in Viti on your phone when safe."
    !servicePresent ->
        "Call service is not connected. Open Viti on your phone when safe."
    !state.serviceDurable ->
        backgroundStartFailureNotice("")
    state.call.legToken.isNotBlank() ->
        "Android Auto is already handling the current Viti call."
    !state.call.voice.registered ->
        "Viti is still connecting service calls. Try again shortly."
    else -> ""
}

internal fun backgroundStartFailureNotice(detail: String): String = buildString {
    append("Call service could not start durably. Open Viti on your phone when safe.")
    detail.takeIf(String::isNotBlank)?.let { append(" ").append(it) }
}
