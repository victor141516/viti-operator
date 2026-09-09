package com.viti.operator.auth

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat
import com.viti.operator.MainActivity
import com.viti.operator.R
import com.viti.operator.data.SettingsStore
import com.viti.operator.data.VitiApi
import com.viti.operator.data.vitiClientIdentity
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch

class RemoteAuthService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var worker: Job? = null
    private var stopDetail = "Agent stopped"
    private var forceClaim = false

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
        startForeground(NOTIFICATION_ID, notification("Waiting for authentication challenges"))
        serviceState.value = ServiceState(true, "Waiting for authentication challenges")
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            releaseAssignmentAndStop(startId)
            return START_NOT_STICKY
        }
        if (intent?.action == ACTION_CLAIM) forceClaim = true
        if (worker?.isActive != true) worker = scope.launch { authLoop() }
        return START_STICKY
    }

    override fun onDestroy() {
        scope.cancel()
        serviceState.value = ServiceState(false, stopDetail)
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private suspend fun authLoop() {
        val engine = SimAuthEngine(this)
        var claimed = false
        while (true) {
            try {
                val identity = vitiClientIdentity()
                val settings = SettingsStore(this).settings.first()
                val api = VitiApi(settings)
                api.heartbeat(identity, authActive = true)
                if (!identity.hasSim) {
                    if (claimed) runCatching { api.setSIMAuthAssignment(identity, false) }
                    stopDetail = "The SIM is no longer available on this phone"
                    stopSelf()
                    return
                }
                if (!claimed) {
                    val assignment = api.simAuthAssignment()
                    if (!forceClaim && assignment.claimedClientId.isNotBlank() && assignment.claimedClientId != identity.id) {
                        stopDetail = "Another phone has taken over authentication"
                        stopSelf()
                        return
                    }
                    api.setSIMAuthAssignment(identity, true)
                    claimed = true
                    forceClaim = false
                    serviceState.value = ServiceState(true, "This phone authenticates the line")
                }
                val assignment = api.simAuthAssignment()
                if (assignment.claimedClientId != identity.id) {
                    stopDetail = "Another phone has taken over authentication"
                    stopSelf()
                    return
                }
                val challenge = api.pollChallenge(identity.id)
                if (challenge == null) {
                    serviceState.value = ServiceState(true, "This phone authenticates the line · waiting for challenges")
                    continue
                }
                Log.i(LOG_TAG, "Authentication challenge received")
                serviceState.value = ServiceState(true, "Resolving a challenge with the SIM")
                try {
                    val material = engine.authenticate(challenge)
                    api.answerChallenge(identity.id, challenge.id, material)
                    Log.i(LOG_TAG, "Authentication response submitted")
                    serviceState.value = ServiceState(true, "Last challenge resolved successfully")
                } catch (error: Exception) {
                    runCatching { api.answerChallengeError(identity.id, challenge.id) }
                    throw error
                }
            } catch (cancelled: CancellationException) {
                throw cancelled
            } catch (error: Exception) {
                Log.e(LOG_TAG, "Authentication loop failed", error)
                serviceState.value = ServiceState(true, error.message ?: "Authentication error")
                delay(3_000)
            }
        }
    }

    private fun releaseAssignmentAndStop(startId: Int) {
        worker?.cancel()
        worker = null
        scope.launch {
            val identity = vitiClientIdentity()
            val result = runCatching {
                val settings = SettingsStore(this@RemoteAuthService).settings.first()
                val api = VitiApi(settings)
                api.heartbeat(identity, authActive = true)
                api.setSIMAuthAssignment(identity, false)
            }
            stopDetail = if (result.isSuccess) {
                "This phone has stopped authenticating the line"
            } else {
                "Agent stopped; the server will switch to the modem when presence expires"
            }
            stopSelfResult(startId)
        }
    }

    private fun notification(detail: String) = NotificationCompat.Builder(this, CHANNEL_ID)
        .setSmallIcon(R.drawable.ic_launcher_foreground)
        .setContentTitle("Viti Operator")
        .setContentText(detail)
        .setOngoing(true)
        .setCategory(NotificationCompat.CATEGORY_SERVICE)
        .setContentIntent(
            PendingIntent.getActivity(
                this,
                0,
                Intent(this, MainActivity::class.java),
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
            ),
        )
        .build()

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            getSystemService(NotificationManager::class.java).createNotificationChannel(
                NotificationChannel(CHANNEL_ID, "Remote SIM authentication", NotificationManager.IMPORTANCE_LOW),
            )
        }
    }

    data class ServiceState(val running: Boolean = false, val detail: String = "Agent stopped")

    companion object {
        private const val CHANNEL_ID = "remote_sim_auth"
        private const val LOG_TAG = "VitiRemoteAuth"
        private const val NOTIFICATION_ID = 4102
        const val ACTION_STOP = "com.viti.operator.STOP_REMOTE_AUTH"
        const val ACTION_CLAIM = "com.viti.operator.CLAIM_REMOTE_AUTH"

        private val serviceState = MutableStateFlow(ServiceState())
        val state = serviceState.asStateFlow()
    }
}
